const fs=require('fs'),vm=require('vm'),assert=require('assert');
class Node {
 constructor(tag,attrs={},kids=[]){this.tagName=tag.toUpperCase();this.attrs=attrs;this.children=[];this.listeners={};this.style={};this.value='';this.classList={replace(){}};this.append(...kids);}
 append(...nodes){for(const node of nodes.flat()){if(node instanceof Node){node.parentElement=this;this.children.push(node);}}}
 appendChild(n){this.append(n);}
 replaceChildren(...n){this.children=[];this.append(...n);}
 addEventListener(k,f){this.listeners[k]=f;}
 matches(s){return s.startsWith('.')?(this.attrs.class||'').split(' ').includes(s.slice(1)):this.tagName===s.toUpperCase();}
 querySelectorAll(s){return this.children.flatMap(n=>[...(n.matches(s)?[n]:[]),...n.querySelectorAll(s)]);}
 querySelector(s){return this.querySelectorAll(s)[0]||null;}
 closest(s){return this.matches(s)?this:this.parentElement?.closest(s);}
 focus(){}
}
function E(tag,attrs,...kids){return new Node(tag,attrs||{},kids);}
(async()=>{
 const context={E,Blob,baseclass:{extend:o=>o},_:s=>s};vm.createContext(context);vm.runInContext("String.prototype.format=function(...args){let i=0;return this.replace(/%s/g,()=>args[i++]);}",context);
 const editor=vm.runInContext('(function(){'+fs.readFileSync(__dirname+'/../files/luci/config-editor.js','utf8')+'})()',context);
 const initial={revision:'1',device_scope:{mode:'all',addresses:[]},dns_rules:[],h2_sites:[{domain:'b.a.com',cert:'',key:'',h2_routes:[{path:'/c',json:'{"n":9007199254740993}'}]},{domain:'h.g.com',cert:'',key:'',h2_routes:[{path:'/c',json:'{"n":2}'}]}]};
 let draft;const form=editor.create(initial,true,async text=>{draft=JSON.parse(text);return initial});
 let cards=form.node.querySelectorAll('.fsd-site-card');assert.equal(cards.length,2);assert.equal(cards[0].querySelectorAll('.fsd-route-card').length,1);
 cards[0].querySelector('.fsd-site-domain').value='renamed.a.com';
 // The first button in the editor is Save.
 await form.node.querySelectorAll('button')[0].listeners.click();
 assert(draft);assert(!Object.hasOwn(draft,'h2_routes'));assert.equal(draft.h2_sites[0].source,'b.a.com');assert.equal(draft.h2_sites[0].domain,'renamed.a.com');assert.equal(draft.h2_sites[0].h2_routes[0].source,'/c');assert.equal(draft.h2_sites[1].h2_routes[0].source,'/c');
 cards=form.node.querySelectorAll('.fsd-site-card');const route=cards[1].querySelector('.fsd-route-card');const raw=route.querySelector('.fsd-json-raw');raw.value='{"n":9007199254740995}';form.node.listeners.input({target:raw});
 await form.node.querySelectorAll('button')[0].listeners.click();assert.equal(draft.h2_sites[1].h2_routes[0].json,'{"n":9007199254740995}');assert.equal(draft.h2_sites[0].h2_routes[0].source,'/c');
 console.log('PASS: nested site rendering, independent route drafts, rename source, exact JSON text');
})().catch(e=>{console.error(e);process.exit(1)});
