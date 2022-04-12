/**
 * Test the presence of fetch and async execution
 *
 *
 */
let test = async () => {
  return await (await fetch('https://api.myip.com/')).json();
}

(async function() {
  console.log(await test())
})()
