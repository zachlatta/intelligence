package web

import (
	"os/exec"
	"testing"
)

func TestAttachmentPickerBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	out, err := exec.Command(node, "-e", attachmentPickerHarness, "attachments.js").CombinedOutput()
	if err != nil {
		t.Fatalf("attachment picker: %v\n%s", err, out)
	}
}

const attachmentPickerHarness = `
const assert = require('assert');
class El {
  constructor(tag) { this.tag=tag; this.children=[]; this.textContent=''; this.classList={add(){},remove(){}}; }
  append(...xs) { xs.forEach(x => x.parent=this); this.children.push(...xs); }
  prepend(...xs) { xs.forEach(x => x.parent=this); this.children.unshift(...xs); }
  after(x) { this.sibling=x; }
  setAttribute(k,v) { this[k]=v; }
  addEventListener(k,f) { this[k]=f; }
  replaceChildren() { this.children=[]; }
  remove() { this.parent.children=this.parent.children.filter(x=>x!==this); }
  querySelectorAll(tag) { return this.children.flatMap(x=>[...(x.tag===tag?[x]:[]), ...x.querySelectorAll(tag)]); }
  contains() { return false; }
  focus() { this.focused=true; }
  click() { this.clicked=true; }
}
global.document={createElement:t=>new El(t)};
global.window={addEventListener(){}};
let revoked=0; global.URL={createObjectURL:()=> 'blob:preview',revokeObjectURL:()=>revoked++};
global.FileReader=class {readAsDataURL(){this.result='data:image/png;base64,aW1n'; this.onload();}};
let uploads=0; global.fetch=async()=>({ok:true,json:async()=>({reference:'attachment:'+(++uploads)})});
eval(require('fs').readFileSync(process.argv[1],'utf8'));
const text=new El('textarea'), zone=new El('section'), panel=new El('div'), action=new El('span');
const picker=imageAttachments(text,zone,{panel,action});
const otherText=new El('textarea');
imageAttachments(otherText,new El('section'));
assert.equal(otherText.sibling.children[0].className,'attachment-actions','other forms keep their attach action');
const [actions]=action.children, [pick,input,help]=actions.children, [previews,error]=panel.children;
pick.onclick(); assert(input.clicked,'button opens picker');
const file=(name='Screenshot with a very long filename.png',size=10,type='image/png')=>({name,size,type});
input.files=[file()]; input.onchange();
assert(picker.hasImages());
const [img,name,remove]=previews.children[0].children;
assert.equal(remove.textContent,'×'); assert.equal(remove['aria-label'],'Remove '+name.textContent);
assert.equal(name.title,name.textContent); assert.equal(img.alt,name.textContent);
picker.setDisabled(true); assert(remove.disabled && pick.disabled);
remove.onclick(); assert.equal(previews.children.length,1,'locked remove does nothing');
picker.setDisabled(false); remove.onclick(); assert.equal(revoked,1); assert(!picker.hasImages()); assert(pick.focused);
let prevented=0;
text.paste({clipboardData:{files:[file('bad.svg',10,'image/svg+xml')]},preventDefault(){prevented++}});
assert.equal(prevented,1); assert(error.textContent.includes('PNG')); assert(!picker.hasImages());
zone.drop({dataTransfer:{files:[file('big.png',4*1024*1024+1)]},preventDefault(){}});
assert(!picker.hasImages()); assert(error.textContent.includes('4 MiB'));
zone.drop({dataTransfer:{files:Array.from({length:9},()=>file())},preventDefault(){}});
assert.equal(previews.children.length,8,'count limit');
picker.clear(); assert(!picker.hasImages());
text.paste({clipboardData:{files:[file('a.png',4*1024*1024),file('b.png',4*1024*1024),file('c.png',1)]},preventDefault(){}});
assert.equal(previews.children.length,2,'total size limit');
(async()=>{
 const prompt=await picker.prompt('task'); assert.equal(uploads,2); assert(prompt.includes('attachment:1'));
 await picker.prompt('retry'); assert.equal(uploads,2,'retry reuses uploads');
 picker.clear(); assert(!picker.hasImages());
})();
`
