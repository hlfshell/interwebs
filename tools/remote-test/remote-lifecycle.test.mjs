import {test} from 'node:test';
import assert from 'node:assert/strict';
import {cleanupOnce,finalizeRun,cleanupResources,provisioningGuard} from './remote-lifecycle.mjs';

test('log failure cannot prevent cleanup',async()=>{
  let cleaned=false;
  await assert.rejects(finalizeRun(async()=>{throw Error('disk full');},async()=>{cleaned=true;}),AggregateError);
  assert.equal(cleaned,true);
});
test('overlapping shutdown callers wait for the same cleanup',async()=>{
  let release,calls=0;const gate=new Promise(r=>release=r);
  const finish=cleanupOnce(async()=>{calls++;await gate;});const a=finish(),b=finish();assert.equal(a,b);
  let completed=false;b.then(()=>completed=true);await Promise.resolve();assert.equal(completed,false);release();await Promise.all([a,b]);assert.equal(calls,1);
});
function environment({failID,tagOnly=false}={}){
  const m={tag:'test-run'};let droplets=tagOnly?[]:[{id:1,tags:['test-run']},{id:2,tags:['test-run']}];let keys=[{id:3,name:'test-run'},{id:4,name:'unrelated'}];let tags=[{name:'test-run'}];const deleted=[];
  const deps={pause:async()=>{},save:async()=>{},doctl:async(_group,kind)=>structuredClone(kind==='droplet'?droplets:kind==='ssh-key'?keys:tags),command:async(_cmd,args)=>{
    const kind=args[1],id=args[3];if(id===String(failID))throw Error('API unavailable for '+id);deleted.push(id);
    if(kind==='droplet')droplets=droplets.filter(d=>String(d.id)!==id);if(kind==='ssh-key')keys=keys.filter(k=>String(k.id)!==id);if(kind==='tag')tags=[];
  }};return{m,deps,deleted};
}
test('partial provisioning is reconciled by tag without recorded IDs',async()=>{
  const e=environment();await cleanupResources(e.m,e.deps);assert.deepEqual(e.deleted,['1','2','3','test-run']);assert.ok(e.m.cleanedAt);
});
test('one API failure does not skip other resources or mark cleanup complete',async()=>{
  const e=environment({failID:1});await assert.rejects(cleanupResources(e.m,e.deps),AggregateError);assert.deepEqual(e.deleted,['2','3']);assert.equal(e.m.cleanedAt,undefined);assert.ok(e.m.cleanupErrors.some(s=>s.includes('1')));
});
test('key-only provisioning failure is cleaned safely',async()=>{
  const e=environment({tagOnly:true});await cleanupResources(e.m,e.deps);assert.deepEqual(e.deleted,['3','test-run']);
});
test('both diagnostics and cleanup failures are reported',async()=>{
  await assert.rejects(finalizeRun(async()=>{throw Error('log');},async()=>{throw Error('cleanup');}),e=>e.errors.length===2);
});
test('shutdown waits for in-flight provisioning and rejects later creation',async()=>{
  const guard=provisioningGuard();let release;const created=guard.track(()=>new Promise(r=>release=r));await Promise.resolve();let drained=false;const shutdown=guard.drain().then(()=>drained=true);await Promise.resolve();assert.equal(drained,false);await assert.rejects(guard.track(()=>{}));release();await created;await shutdown;assert.equal(drained,true);
});
