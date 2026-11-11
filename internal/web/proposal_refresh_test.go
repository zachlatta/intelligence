package web

import (
	"os/exec"
	"strings"
	"testing"
)

func TestProposalRepliesRefreshWithoutLosingDraft(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	_, src, found := strings.Cut(proposalsHTML, "<script>\n// Use the run page's")
	if !found {
		t.Fatal("missing thread refresh script")
	}
	src, _, _ = strings.Cut(src, "</script>")
	out, err := exec.Command(node, "-e", proposalRefreshHarness, "// Use the run page's"+src).CombinedOutput()
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
}

const proposalRefreshHarness = `
const assert=require('assert');
const text={value:'An unfinished follow-up',readOnly:false},button={disabled:false};
const form={querySelector:sel=>sel==='button'?button:text};
const els={'proposal-comment':form,'comment-error':{textContent:''},'thread-update':{textContent:''},discussion:{addEventListener:()=>{}}};
function thread(html){return {innerHTML:html,replaceWith(next){els['proposal-thread']=next;}};}
els['proposal-thread']=thread('Question');
let interval;
global.document={hidden:false,getElementById:id=>els[id],addEventListener:()=>{}};
global.location={href:'https://example.com/workflow-proposals/demo'};
global.setInterval=(fn,ms)=>{assert.strictEqual(ms,4000);interval=fn;};
global.DOMParser=class {parseFromString(html){return {getElementById:()=>thread(html)};}};
let gets=0,posts=0,failPost=false,release;
global.fetch=async(url,init)=>{
 if(init.method==='POST'){posts++;return {ok:!failPost,text:async()=>'Could not dispatch'};}
 gets++;
 return new Promise(resolve=>{release=()=>resolve({ok:true,text:async()=>'<p>Automatic answer</p>'});});
};
eval(process.argv[1]);
const tick=()=>new Promise(resolve=>setTimeout(resolve,0));
(async()=>{
 const poll=interval();await tick();const again=interval();await again;
 assert.strictEqual(gets,1,'overlapping polls must not start more requests');
 release();await poll;
 assert(els['proposal-thread'].innerHTML.includes('Automatic answer'));
 assert.strictEqual(text.value,'An unfinished follow-up','refresh erased draft');
 assert.strictEqual(els['proposal-comment'],form,'refresh replaced form');
 failPost=true;
 await form.onsubmit({preventDefault(){},currentTarget:form});
 assert.strictEqual(text.value,'An unfinished follow-up','failed dispatch erased draft');
 assert.strictEqual(button.disabled,false);
 assert.strictEqual(text.readOnly,false);
 assert.strictEqual(els['comment-error'].textContent,'Could not dispatch');
 failPost=false;
 const sent=form.onsubmit({preventDefault(){},currentTarget:form});await tick();release();await sent;
 assert.strictEqual(text.value,'','confirmed question was not cleared');
 assert.strictEqual(posts,2);
 assert.strictEqual(els['comment-error'].textContent,'');
 assert.strictEqual(button.disabled,false);
})().catch(error=>{console.error(error);process.exit(1);});
`
