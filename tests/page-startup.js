const fs = require('fs'), vm = require('vm'), assert = require('assert');
const source = fs.readFileSync(require('path').join(__dirname, '../files/luci/overview.js'), 'utf8');
const load = source.match(/load: function\(\) \{([\s\S]*?)\n\t\},\n\n\trender:/)[1];
(async () => {
 for (const protocol of ['http:', 'https:']) {
  for (const state of [{enabled:false,running:false}, {enabled:true,running:false}, {enabled:true,running:true}]) {
   let calls=0;
   const ctx={installation:async()=>state,status:async()=>{calls++;return{};},config:async()=>({}),readEditor:async()=>({}),profiles:async()=>({active:"config.yaml"}),readDocument:async()=>({}),window:{location:{protocol,replace:()=>{throw Error('Unexpected redirect');}}}};
   const result=await vm.runInNewContext('(function(){'+load+'})()',ctx);
   assert.equal(calls, state.enabled && state.running ? 1:0);
   if (!state.enabled || !state.running) assert.deepEqual(result.installation,state);
   else assert.equal(result.length,4);
  }
 }
 console.log('PASS: HTTP/HTTPS first install, stopped service and running service without redirects');
})().catch(e=>{console.error(e);process.exit(1);});
