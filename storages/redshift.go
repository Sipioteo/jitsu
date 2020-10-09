package storages

import (
	"context"
	"fmt"
	"github.com/ksensehq/eventnative/adapters"
	"github.com/ksensehq/eventnative/appconfig"
	"github.com/ksensehq/eventnative/events"
	"github.com/ksensehq/eventnative/logging"
	"github.com/ksensehq/eventnative/schema"
	"strings"
	"time"
)

const tableFileKeyDelimiter = "-table-"

//Store files to aws RedShift in two modes:
//batch: via aws s3 in batch mode (1 file = 1 transaction)
//stream: via events queue in stream mode (1 object = 1 transaction)
type AwsRedshift struct {
	name            string
	s3Adapter       *adapters.S3
	redshiftAdapter *adapters.AwsRedshift
	tableHelper     *TableHelper
	schemaProcessor *schema.Processor
	streamingWorker *StreamingWorker
	breakOnError    bool

	closed bool
}

//NewAwsRedshift return AwsRedshift and start goroutine for aws redshift batch storage or for stream consumer depend on destination mode
func NewAwsRedshift(ctx context.Context, name string, eventQueue *events.PersistentQueue, s3Config *adapters.S3Config, redshiftConfig *adapters.DataSourceConfig,
	processor *schema.Processor, breakOnError, streamMode bool, monitorKeeper MonitorKeeper) (*AwsRedshift, error) {
	var s3Adapter *adapters.S3
	if !streamMode {
		var err error
		s3Adapter, err = adapters.NewS3(s3Config)
		if err != nil {
			return nil, err
		}
	}

	redshiftAdapter, err := adapters.NewAwsRedshift(ctx, redshiftConfig, s3Config)
	if err != nil {
		return nil, err
	}

	//create db schema if doesn't exist
	err = redshiftAdapter.CreateDbSchema(redshiftConfig.Schema)
	if err != nil {
		redshiftAdapter.Close()
		return nil, err
	}

	tableHelper := NewTableHelper(redshiftAdapter, monitorKeeper, RedshiftType)

	ar := &AwsRedshift{
		name:            name,
		s3Adapter:       s3Adapter,
		redshiftAdapter: redshiftAdapter,
		tableHelper:     tableHelper,
		schemaProcessor: processor,
		breakOnError:    breakOnError,
	}

	if streamMode {
		ar.streamingWorker = newStreamingWorker(eventQueue, processor, ar)
		ar.streamingWorker.start()
	} else {
		ar.startBatch()
	}

	return ar, nil
}

//Periodically (every 30 seconds):
//1. get all files from aws s3
//2. load them to aws Redshift via Copy request
//3. delete file from aws s3
func (ar *AwsRedshift) startBatch() {
	go func() {
		for {
			if ar.closed {
				break
			}
			//TODO configurable
			time.Sleep(30 * time.Second)

			filesKeys, err := ar.s3Adapter.ListBucket(appconfig.Instance.ServerName)
			if err != nil {
				logging.Errorf("[%s] Error reading files from s3: %v", ar.Name(), err)
				continue
			}

			if len(filesKeys) == 0 {
				continue
			}

			for _, fileKey := range filesKeys {
				names := strings.Split(fileKey, tableFileKeyDelimiter)
				if len(names) != 2 {
					logging.Errorf("[%s] S3 file [%s] has wrong format! Right format: $filename%s$tablename. This file will be skipped.", ar.Name(), fileKey, tableFileKeyDelimiter)
					continue
				}
				wrappedTx, err := ar.redshiftAdapter.OpenTx()
				if err != nil {
					logging.Errorf("[%s] Error creating redshift transaction: %v", ar.Name(), err)
					continue
				}

				if err := ar.redshiftAdapter.Copy(wrappedTx, fileKey, names[1]); err != nil {
					logging.Errorf("[%s] Error copying file [%s] from s3 to redshift: %v", ar.Name(), fileKey, err)
					wrappedTx.Rollback()
					continue
				}

				wrappedTx.Commit()
				//TODO may be we need to have a journal for collecting already processed files names
				// if ar.s3Adapter.DeleteObject fails => it will be processed next time => duplicate data
				if err := ar.s3Adapter.DeleteObject(fileKey); err != nil {
					logging.Errorf("[%s] System error: file %s wasn't deleted from s3 and will be inserted in db again: %v", ar.Name(), fileKey, err)
					continue
				}

			}
		}
	}()
}

//Insert fact in Redshift
func (ar *AwsRedshift) Insert(dataSchema *schema.Table, fact events.Fact) (err error) {
	dbSchema, err := ar.tableHelper.EnsureTable(ar.Name(), dataSchema)
	if err != nil {
		return err
	}

	if err := ar.schemaProcessor.ApplyDBTypingToObject(dbSchema, fact); err != nil {
		return err
	}

	return ar.redshiftAdapter.Insert(dataSchema, fact)
}

//Store file from byte payload to s3 with processing
func (ar *AwsRedshift) Store(fileName string, payload []byte) error {
	flatData, err := ar.schemaProcessor.ProcessFilePayload(fileName, payload, ar.breakOnError)
	if err != nil {
		return err
	}

	for _, fdata := range flatData {
		dbSchema, err := ar.tableHelper.EnsureTable(ar.Name(), fdata.DataSchema)
		if err != nil {
			return err
		}

		if err := ar.schemaProcessor.ApplyDBTyping(dbSchema, fdata); err != nil {
			return err
		}
	}

	//TODO put them all in one folder and if all ok => move them all to next working folder
	for _, fdata := range flatData {
		err := ar.s3Adapter.UploadBytes(fdata.FileName+tableFileKeyDelimiter+fdata.DataSchema.Name, fdata.GetPayloadBytes(schema.JsonMarshallerInstance))
		if err != nil {
			return err
		}
	}

	return nil
}

func (ar *AwsRedshift) Name() string {
	return ar.name
}

func (ar *AwsRedshift) Type() string {
	return RedshiftType
}

func (ar *AwsRedshift) Close() error {
	ar.closed = true

	if err := ar.redshiftAdapter.Close(); err != nil {
		return fmt.Errorf("[%s] Error closing redshift datasource: %v", ar.Name(), err)
	}

	if ar.streamingWorker != nil {
		ar.streamingWorker.Close()
	}

	return nil
}
