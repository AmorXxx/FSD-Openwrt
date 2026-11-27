const fs=require('fs'),vm=require('vm'),assert=require('assert'),path=require('path');
const source=fs.readFileSync(path.join(__dirname,'../files/luci/logging.js'),'utf8');
function element(tag,attrs,children){
 if(Array.isArray(attrs)||typeof attrs==='string'){children=attrs;attrs={};}
 return {tag,attrs:attrs||{},children:children||[],value:attrs&&attrs.value||'',listeners:{},setAttribute(k,v){this.attrs[k]=v;},addEventListener(k,f){this.listeners[k]=f;},replaceChildren(...c){this.children=c;}};
}
function flatten(node){return !node||typeof node!=='object'?[]:[node,...(Array.isArray(node.children)?node.children:[node.children]).flatMap(flatten)];}
(async()=>{
 for(const writable of [true,false]){
  let state={config:{enabled:true,local_csv:false,endpoint:'https://example.com/v1/events',device_id:'test'},token_present:true,upload:{enabled:true},csv:{enabled:false}},requests=[],poller;
  const ctx={E:element,_:s=>s,L:{hasViewPermission:()=>writable},window:{location:{protocol:'https:'}},view:{extend:o=>o},ui:{addNotification(){throw Error('Unexpected notification')}},poll:{add:f=>{poller=f;}},rpc:{declare:({method})=>async body=>{requests.push([method,body]);if(method==='test_logging')return{message:'checked'};if(method==='save_logging'){state.config=JSON.parse(body);state.csv.enabled=state.config.local_csv;state.upload.enabled=state.config.enabled;}return state;}}};
  const page=vm.runInNewContext('(function(){'+source+'})()',ctx),tree=page.render(await page.load()),nodes=flatten(tree),inputs=nodes.filter(n=>n.tag==='input'),buttons=nodes.filter(n=>n.tag==='button');
  assert.equal(inputs.length,7);assert.equal(inputs[0].checked,false);assert.equal(inputs[1].checked,true);assert.equal(inputs[4].value,'');assert.equal(buttons[0].disabled,!writable);
  if(writable){
   inputs[0].checked=true;inputs[1].checked=false;inputs[4].value='replacement-secret';inputs[6].checked=true;
   await buttons[0].listeners.click();const payload=JSON.parse(requests.find(r=>r[0]==='save_logging')[1]);assert.equal(payload.tls_keylog,true);assert.equal(payload.local_csv,true);assert.equal(payload.enabled,false);assert.equal(payload.token,'replacement-secret');assert.equal(inputs[4].value,'');
   inputs[2].value='https://draft.example/v1/events';await poller();assert.equal(inputs[2].value,'https://draft.example/v1/events');
   await buttons[1].listeners.click();assert(requests.some(r=>r[0]==='test_logging'));
  }
 }
 const acl=JSON.parse(fs.readFileSync(path.join(__dirname,'../files/luci/acl.json')));
 assert(acl['luci-app-fsd'].read.ubus.fsd.includes('logging'));
 assert(!acl['luci-app-fsd'].read.ubus.fsd.includes('save_logging'));
 assert(acl['luci-app-fsd'].write.ubus.fsd.includes('save_logging'));
 console.log('PASS: logging page switches, secret clearing, draft-safe polling, permissions and connection test');
})().catch(e=>{console.error(e);process.exit(1);});
