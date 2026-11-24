const fs=require('fs'),vm=require('vm'),assert=require('assert');
const source=fs.readFileSync(require('path').join(__dirname,'../files/luci/overview.js'),'utf8');
const handler=source.match(/profileSelect.addEventListener\('change', async function\(\) \{([\s\S]*?)\n        \}\);/)[1];
(async()=>{
 for(const mode of ['clean','cancel','discard','failure']){
  let reads=0;
  const ctx={profileSelect:{value:'other.yml'},editingName:'active.yaml',editor:{value:mode==='clean'?'old':'draft'},currentYAML:'old',quick:{isDirty:()=>false},profileFeedback:{},
   runConfigAction:fn=>fn(),window:{confirm:()=>mode!=='cancel'},_:x=>x,
   loadDocument:async name=>{reads++;if(mode==='failure')throw Error('read failed');ctx.editingName=name;ctx.editor.value='loaded';}};
  await vm.runInNewContext('(async function(){'+handler+'})()',ctx);
  if(mode==='cancel'){assert.equal(reads,0);assert.equal(ctx.editor.value,'draft');}
  else assert.equal(reads,1);
  assert.equal(ctx.profileSelect.value,mode==='cancel'||mode==='failure'?'active.yaml':'other.yml');
  if(mode==='failure')assert.equal(ctx.profileFeedback.textContent,'read failed');
 }
 console.log('PASS: select loads file without applying, cancel keeps drafts, failed loads restore selection');
})().catch(e=>{console.error(e);process.exit(1)});
