// Share one cleanup promise across normal completion, timeouts and signals.
// Newly imported keys can be listed before Droplet creation accepts them.
// Retry only an explicit rejection, never an ambiguous create timeout.
export async function createWithKeyRetry(create, pause) {
  for(let attempt=0;;attempt++) {
    try {return await create();}
    catch(error) {
      const detail=String(error.stdout||'');
      if(attempt>=5||!detail.includes('422')||!detail.includes('invalid key identifiers'))throw error;
      await pause(10000);
    }
  }
}

export function cleanupDue(manifest, {now=Date.now(), force=false}={}) {
  if(manifest.cleanedAt)return false;
  if(force)return true;
  if(manifest.keepHostRequested)return false;
  return typeof manifest.expiresAt==='number'&&now>manifest.expiresAt;
}

export function cleanupOnce(cleanup) {
  let pending;
  return () => pending ??= Promise.resolve().then(cleanup);
}

// Do not race deletion against a cloud create request that is still in flight.
export function provisioningGuard(){
  const pending=new Set();let closing=false;
  return {
    track(work){if(closing)return Promise.reject(Error('Run is shutting down'));const p=Promise.resolve().then(work);pending.add(p);p.then(()=>pending.delete(p),()=>pending.delete(p));return p;},
    async drain(){closing=true;await Promise.allSettled([...pending]);},
    get closing(){return closing;},
  };
}

// Diagnostics must never prevent resource cleanup. Preserve both failures.
export async function finalizeRun(writeLogs, finish) {
  const errors=[];
  try { await writeLogs(); } catch(error) { errors.push(error); }
  try { await finish(); } catch(error) { errors.push(error); }
  if(errors.length) throw new AggregateError(errors,'Remote run finalization failed');
}

export async function cleanupResources(m,{doctl,command,pause,save,log=()=>{}}) {
  const errors=[];
  const attempt=async fn=>{try{return await fn();}catch(e){errors.push(e);}};
  // A failure deleting one resource must not skip the others.
  const droplets=await attempt(()=>doctl('compute','droplet','list','--tag-name',m.tag));
  for(const d of droplets??[]) await attempt(async()=>{
    if(!d.tags?.includes(m.tag))throw Error(`Refusing untagged droplet ${d.id}`);
    await command('doctl',['compute','droplet','delete',String(d.id),'--force']);
  });
  let remaining;
  await attempt(async()=>{
    for(let n=0;n<12;n++){
      remaining=await doctl('compute','droplet','list','--tag-name',m.tag);
      if(!remaining.length)return;
      if(n<11)await pause(5000);
    }
    throw Error(`Droplets remain: ${remaining.map(d=>d.id).join(', ')}`);
  });
  const keys=await attempt(()=>doctl('compute','ssh-key','list'));
  // The import receipt is authoritative even if the list endpoint has not
  // caught up. Otherwise a failed create can leave a key that appears later.
  const keyIDs=new Set((keys??[]).filter(key=>key.name===m.tag).map(key=>key.id));
  if(m.keyID)keyIDs.add(m.keyID);
  for(const id of keyIDs)if(!m.deletedKeyIDs?.includes(id))await attempt(async()=>{
    await command('doctl',['compute','ssh-key','delete',String(id),'--force']);
    (m.deletedKeyIDs??=[]).push(id);
    await save(m);
  });
  await attempt(async()=>{
    for(let n=0;n<12;n++) {
      const left=(await doctl('compute','ssh-key','list')).filter(k=>k.name===m.tag);
      if(!left.length)return;
      if(n===11)throw Error(`SSH keys remain: ${left.map(k=>k.id).join(', ')}`);
      await pause(5000);
    }
  });
  // Keep the discovery tag while any deletion is uncertain, for later recovery.
  if(errors.length){m.cleanupErrors=errors.map(e=>e.message);await attempt(()=>save(m));throw new AggregateError(errors,`Cleanup incomplete for ${m.tag}`);}
  const tags=await doctl('compute','tag','list');
  if(tags.some(t=>t.name===m.tag))await command('doctl',['compute','tag','delete',m.tag,'--force']);
  if((await doctl('compute','tag','list')).some(t=>t.name===m.tag))throw Error(`Tag remains: ${m.tag}`);
  m.cleanedAt=new Date().toISOString();delete m.cleanupErrors;await save(m);log(`Verified cleanup of ${m.tag}`);
}
