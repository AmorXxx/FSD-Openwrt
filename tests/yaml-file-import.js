const fs = require('fs'), vm = require('vm'), assert = require('assert');
const source = fs.readFileSync(require('path').join(__dirname, '../files/luci/overview.js'), 'utf8');
const handler = source.match(/file.addEventListener\('change', async function\(\) \{([\s\S]*?)\n\t\t\}\);/)[1];
(async () => {
 for (const mode of ['success', 'duplicate', 'dirty']) {
  let request, message;
  const editor = {value: mode === 'dirty' ? 'draft' : 'current'};
  const ctx = { file:{files:[{name:'home.v2.yml',size:20,text:async()=> 'new yaml'}],value:'file'},
   editor, currentYAML:'current', rawReady:true, runConfigAction:fn=>fn(),requireCleanForm:()=>{},
   profileState:{active:'config.yaml'}, profileSelect:{value:'config.yaml'}, profileFeedback:{},
   refreshProfiles:()=>{}, loadDocument:async name=>{ctx.profileSelect.value=name;}, profileAction:async raw=>{request=JSON.parse(raw);if(mode==='duplicate')throw Error('duplicate');return{};},
   _:x=>x, E:()=>{}, text:x=>{message=x;return x;},ui:{addNotification:()=>{}},
  };
  vm.createContext(ctx);
  vm.runInContext("String.prototype.format = function(s) { return this.replace('%s',s); }",ctx);
  await vm.runInContext('(async function(){'+handler+'})()',ctx);
  assert.equal(editor.value,mode==='dirty'?'draft':'current');
  assert.equal(ctx.file.value,'');
  if(mode==='dirty') assert.equal(request,undefined);
  else assert.deepEqual(request,{action:'import',name:'home.v2.yml',active:'config.yaml',yaml:'new yaml'});
  if(mode==='success') assert.equal(ctx.profileSelect.value,'home.v2.yml');
  if(mode==='duplicate') assert.equal(message,'duplicate');
 }
 console.log('PASS: original filename import, duplicate handling, unsaved draft protection');
})().catch(e=>{console.error(e);process.exit(1);});
