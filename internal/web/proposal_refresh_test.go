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
	for _, pending := range []string{"true", "false"} {
		out, err := exec.Command(node, "-e", proposalRefreshHarness, "// Use the run page's"+src, pending).CombinedOutput()
		if err != nil {
			t.Fatalf("refresh (%s): %v\n%s", pending, err, out)
		}
	}
}

const proposalRefreshHarness = `
const assert=require('assert');
const text={value:'An unfinished follow-up',readOnly:false},button={disabled:false};
const form={querySelector:sel=>sel==='button'?button:text};
let onDiscussionClick;
const els={'proposal-comment':form,'comment-error':{textContent:''},'thread-update':{textContent:''},discussion:{addEventListener:(name,fn)=>{if(name==='click')onDiscussionClick=fn;}}};
function thread(html,pending){return {innerHTML:html,dataset:{pending},replaceWith(next){els['proposal-thread']=next;}};}
els['proposal-thread']=thread('Question',process.argv[2]);
let timer,visibility;
global.document={hidden:false,getElementById:id=>els[id],addEventListener:(name,fn)=>{if(name==='visibilitychange')visibility=fn;}};
global.location={href:'https://example.com/workflow-proposals/demo'};
global.setInterval=()=>{throw new Error('unconditional polling interval');};
global.setTimeout=(fn,ms)=>{assert.strictEqual(ms,4000);assert(!timer,'overlapping timers');timer=fn;return 1;};
global.clearTimeout=()=>{timer=null;};
let responsePending='false';
global.DOMParser=class {parseFromString(html){return {getElementById:()=>thread(html,responsePending)};}};
let gets=0,posts=0,failPost=false,release;
global.fetch=async(url,init)=>{
 if(init.method==='POST'){posts++;return {ok:!failPost,text:async()=>'Could not dispatch'};}
 gets++;
 return new Promise(resolve=>{release=()=>resolve({ok:true,text:async()=>'<p>Automatic answer</p>'});});
};
eval(process.argv[1]);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
 if(process.argv[2]==='true'){
  els['comment-error'].textContent='An answer is already queued';
  assert(timer,'pending thread must schedule a poll');
  const poll=timer();await tick();await refreshThread();
  assert.strictEqual(gets,1,'overlapping polls must not start more requests');
  release();await poll;
  assert(els['proposal-thread'].innerHTML.includes('Automatic answer'));
  assert(!timer,'answered thread must stop polling');
  assert.strictEqual(els['comment-error'].textContent,'','resolved answer left a stale 409 error');
 }else{
  assert(!timer,'idle thread must not poll');
  await visibility();assert.strictEqual(gets,0,'idle tab visibility must not poll');
 }
 assert.strictEqual(text.value,'An unfinished follow-up','refresh erased draft');
 assert.strictEqual(els['proposal-comment'],form,'refresh replaced form');
 failPost=true;
 await form.onsubmit({preventDefault(){},currentTarget:form});
 assert.strictEqual(text.value,'An unfinished follow-up','failed dispatch erased draft');
 assert.strictEqual(button.disabled,false);
 assert.strictEqual(text.readOnly,false);
 assert.strictEqual(els['comment-error'].textContent,'Could not dispatch');
 failPost=false;
 // A post can finish after the user switches tabs. It must resume on return,
 // even if the old thread had no pending questions when the post began.
 responsePending='true';
 document.hidden=true;
 await form.onsubmit({preventDefault(){},currentTarget:form});
 assert.strictEqual(els['proposal-thread'].dataset.pending,'true','hidden post lost pending state');
 assert(!timer,'hidden post should not start polling');
 document.hidden=false;const sent=visibility();await tick();release();await sent;
 assert.strictEqual(text.value,'','confirmed question was not cleared');
 assert.strictEqual(posts,2);
 assert.strictEqual(els['comment-error'].textContent,'');
 assert.strictEqual(button.disabled,false);
 assert(timer,'posting a question must restart polling');
 document.hidden=true;await visibility();assert(!timer,'hidden tab must pause polling');
 document.hidden=false;responsePending='false';const back=visibility();await tick();release();await back;await tick();
 assert(!timer,'resolved question must stop polling after returning to tab');
 const retryBtn={disabled:false,dataset:{retryAnswer:'7'},closest:sel=>sel==='[data-retry-answer]'?retryBtn:null};
 failPost=true;
 await onDiscussionClick({target:retryBtn});
 assert.strictEqual(retryBtn.disabled,false);
 assert.strictEqual(els['comment-error'].textContent,'Could not dispatch');
 failPost=false;responsePending='true';
 const retried=onDiscussionClick({target:retryBtn});await tick();release();await retried;
 assert.strictEqual(els['comment-error'].textContent,'','successful retry left a stale error while pending');
 clearTimeout(timer);
})().catch(error=>{console.error(error);process.exit(1);});
`
