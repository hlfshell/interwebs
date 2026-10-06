// Opt-in real-network test: one short-lived cloud publisher, one owned local
// receiver, no injected peers or metadata. Everything billed is uniquely tagged.
import {spawn,execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {mkdir,readFile,writeFile,rename,stat,appendFile,readdir} from 'node:fs/promises';
import {resolve,join,dirname} from 'node:path';
import {randomUUID} from 'node:crypto';
import {cleanupOnce,cleanupResources,provisioningGuard,createWithKeyRetry} from './remote-lifecycle.mjs';
import {checkBrowser,checkRange} from './hosted-browser.mjs';

const root=resolve(import.meta.dirname,'../..');
const args=process.argv.slice(2);
const keepHost=args.includes('--keep-host');
const measureDiscovery=args.includes('--measure-discovery');
if(args.some(arg=>arg.startsWith('--')&&!['--keep-host','--measure-discovery'].includes(arg)))throw Error('Unknown option');
const source=resolve(args.find(arg=>!arg.startsWith('--'))||join(root,'test_sites/blog'));
const tag=`interweb-test-${Date.now()}-${randomUUID()}`;
const dir=join(root,'tools/.remote-runs',tag);
await mkdir(dir,{recursive:true,mode:0o700});
const exec=promisify(execFile);
const command=async(name,args,options={})=>(await exec(name,args,{cwd:root,timeout:60000,maxBuffer:8<<20,...options})).stdout;
const doctl=async(...args)=>JSON.parse(await command('doctl',[...args,'--output','json']));
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const manifest={tag,expiresAt:keepHost?null:Date.now()+45*60*1000,keepHostRequested:keepHost,droplets:[],results:{}};
const save=async m=>{await writeFile(join(dir,'manifest.json.tmp'),JSON.stringify(m,null,2),{mode:0o600});await rename(join(dir,'manifest.json.tmp'),join(dir,'manifest.json'));};
await save(manifest);
console.log('Run:',dir);
const provisioning=provisioningGuard();
let receiver,receiverExit,ip,hostRetained=false;
async function stopReceiver(){if(!receiver)return;const child=receiver;receiver=null;child.kill('SIGTERM');const timer=setTimeout(()=>child.kill('SIGKILL'),10000);try{await receiverExit;}finally{clearTimeout(timer);}}
const finish=cleanupOnce(async()=>{await provisioning.drain();await stopReceiver();await cleanupResources(manifest,{doctl,command,pause,save,log:console.log});});
for(const signal of ['SIGINT','SIGTERM'])process.once(signal,()=>{(hostRetained?stopReceiver():finish()).then(()=>process.exit(130)).catch(e=>{console.error(e);process.exit(1);});});
const deadline=setTimeout(()=>{console.error('45-minute test deadline');(hostRetained?stopReceiver():finish()).then(()=>process.exit(1)).catch(e=>{console.error(e);process.exit(1);});},45*60*1000);
const options=['-F','/dev/null','-i',join(dir,'key'),'-o','IdentitiesOnly=yes','-o','BatchMode=yes','-o','ConnectTimeout=8','-o','ServerAliveInterval=15','-o','ServerAliveCountMax=3','-o','StrictHostKeyChecking=accept-new','-o',`UserKnownHostsFile=${join(dir,'known_hosts')}`];
const ssh=cmd=>command('ssh',[...options,`root@${ip}`,cmd]);
async function until(label,fn,seconds=240,interval=5000){const end=Date.now()+seconds*1000;let last;while(Date.now()<end){if(provisioning.closing)throw Error('Shutting down');try{const result=await fn();if(result)return result;}catch(e){last=e.message;}await pause(interval);}throw Error(`${label} timed out: ${last??'not ready'}`);}
const localStatus=async()=>JSON.parse(await readFile(join(dir,'receiver-data/runner-status.json'),'utf8'));
const remoteStatus=async()=>JSON.parse(await ssh('cat /root/interweb-data/runner-status.json'));
async function startReceiver(magnet,hosting){
  await writeFile(join(dir,'receiver.yaml'),JSON.stringify({data_dir:join(dir,'receiver-data'),offline:false,sites:[{name:'blog',magnet,hosting,serve:{listen:'127.0.0.1:0'}}]}),{mode:0o600});
  receiver=spawn(join(dir,'interweb-local'),['run','--json','--config',join(dir,'receiver.yaml')],{stdio:['ignore','pipe','pipe']});
  receiverExit=new Promise((resolve,reject)=>{receiver.once('exit',resolve);receiver.once('error',reject);});
  receiver.stdout.on('data',()=>{});
  receiver.stderr.on('data',b=>appendFile(join(dir,'receiver-errors.log'),b).catch(()=>{}));
}
try {
  await stat(join(source,'index.html'));
  await command('go',['build','-o',join(dir,'interweb-local'),'./cmd/interweb'],{cwd:join(root,'interwebs-hosted'),env:{...process.env,GOWORK:'off'},timeout:180000});
  await command('go',['build','-o',join(dir,'interweb-linux'),'./cmd/interweb'],{cwd:join(root,'interwebs-hosted'),env:{...process.env,GOWORK:'off',CGO_ENABLED:'0',GOOS:'linux',GOARCH:'amd64'},timeout:180000});
  const size=(await doctl('compute','size','list')).find(s=>s.slug==='s-1vcpu-1gb'&&s.available&&s.regions.includes('nyc3'));
  if(!size||size.price_hourly>0.02)throw Error('No approved low-cost droplet');
  manifest.hourlyCost=size.price_hourly;await save(manifest);
  await command('ssh-keygen',['-t','ed25519','-N','','-f',join(dir,'key'),'-C',tag]);
  const [key]=await provisioning.track(()=>doctl('compute','ssh-key','import',tag,'--public-key-file',join(dir,'key.pub')));
  manifest.keyID=key.id;await save(manifest);
  // Allow imported keys to become visible to droplet provisioning.
  await pause(5000);
  const [droplet]=await createWithKeyRetry(()=>provisioning.track(()=>doctl('compute','droplet','create',tag,'--size',size.slug,'--image','ubuntu-24-04-x64','--region','nyc3','--ssh-keys',String(key.id),'--tag-name',tag)),pause);
  manifest.droplets.push({id:droplet.id});await save(manifest);
  ip=await until('VM readiness',async()=>{const [d]=await doctl('compute','droplet','get',String(droplet.id));return d.status==='active'&&d.networks.v4.find(n=>n.type==='public')?.ip_address;});
  manifest.droplets[0].ip=ip;await save(manifest);
  await until('SSH and cloud initialization',()=>ssh('test -f /var/lib/cloud/instance/boot-finished').then(()=>true));console.log('VM ready:',ip);
  await ssh('command -v rsync || (apt-get update -qq && apt-get install -y -qq rsync)');
  await command('tar',['-czf',join(dir,'site.tar.gz'),'-C',source,'.'],{timeout:180000});
  await writeFile(join(dir,'remote.yaml'),JSON.stringify({data_dir:'/root/interweb-data',source_roots:['/root/blog'],offline:false,sites:[{name:'blog',folder:'/root/blog',hosting:true,serve:{listen:'127.0.0.1:8082'}}]}),{mode:0o600});
  console.log('Uploading blog');
  const remoteShell=['ssh',...options].map(s=>`'${s.replaceAll("'", "'\\''")}'`).join(' ');
  for(let attempt=1;attempt<=3;attempt++) {
    try {
      await command('rsync',['--partial','--append-verify','--timeout=60','-e',remoteShell,join(dir,'interweb-linux'),join(dir,'site.tar.gz'),join(dir,'remote.yaml'),`root@${ip}:/root/`],{timeout:600000});
      break;
    }catch(error){if(attempt===3)throw error;console.log('Upload interrupted; resuming attempt',attempt+1);await pause(5000);}
  }
  const lifetime=keepHost?'--property=Restart=on-failure':'--property=RuntimeMaxSec=2400';
  await ssh(`mkdir /root/blog && tar -xzf /root/site.tar.gz -C /root/blog && chmod 700 /root/interweb-linux && systemd-run --unit=interweb-test ${lifetime} /root/interweb-linux run --json --config /root/remote.yaml`);
  const began=Date.now();let lastReport=0;
  const published=await until('remote publication',async()=>{const s=await remoteStatus();if(Date.now()-lastReport>30000){console.log('Publishing:',s.sites[0].phase,Math.round((Date.now()-began)/1000),'seconds');lastReport=Date.now();}return s.sites[0].phase==='ready'&&s.sites[0].state.Core.AnnouncedSequence>0&&s;},1200);
  manifest.results.publicationMs=Date.now()-began;
  const magnet=published.sites[0].state.Core.Magnet;
  manifest.magnet=magnet;manifest.hash=published.sites[0].state.Core.Current;await save(manifest);
  if(keepHost) { hostRetained=true; manifest.retainedAt=new Date().toISOString(); await save(manifest); }
  if(keepHost&&!measureDiscovery) {
    await ssh('curl --fail --silent --output /dev/null http://127.0.0.1:8082/');
    manifest.retainedAt=new Date().toISOString();await save(manifest);
    hostRetained=true;
    console.log('Publisher left running until requested shutdown:',JSON.stringify({ip,magnet,manifest:join(dir,'manifest.json'),hourlyCost:manifest.hourlyCost}));
  } else {
  console.log('Published; starting cold receiver with signed magnet only:',magnet);
  const discoveryStart=Date.now();await startReceiver(magnet,false);
  const ready=await until('cold signed discovery and index',async()=>{const s=await localStatus();return s.event!=='stopped'&&s.sites[0].phase==='ready'&&s.sites[0].state.Core.Current===manifest.hash&&s;},360);
  const origin=ready.sites[0].url;
  manifest.results.coldView={elapsedMs:Date.now()-discoveryStart,bytes:ready.sites[0].state.Core.Bytes,total:ready.sites[0].state.Core.Total,origin,discovery:ready.sites[0].state.Core.Discovery};await save(manifest);
  console.log('Cold view ready:',manifest.results.coldView);
  manifest.results.staticBrowser=await checkBrowser(origin,source,join(dir,'homepage-static.png'),{blockVideo:true});await save(manifest);
  console.log('Non-video homepage:',manifest.results.staticBrowser.localAssetsPass,manifest.results.staticBrowser.loadMs,'ms');
  manifest.results.normalBrowser=await checkBrowser(origin,source,join(dir,'homepage-normal.png'));await save(manifest);
  console.log('Unmodified homepage:',manifest.results.normalBrowser.localAssetsPass,manifest.results.normalBrowser.loadMs,'ms');
  const video='/videos/poprocks/devx.mp4';const videoSize=(await stat(join(source,video))).size;
  manifest.results.videoRanges=[await checkRange(origin,source,video,0,1<<20),await checkRange(origin,source,video,videoSize-(64<<10),64<<10)];await save(manifest);
  console.log('Video range checks:',manifest.results.videoRanges);
  await writeFile(join(dir,'peer-sockets.txt'),(await command('ss',['-tanp'])).split('\n').filter(l=>l.includes(ip)||l.includes('interweb')).join('\n'));
  if(measureDiscovery) {
    console.log('Waiting for confirmed DHT contacts to be checkpointed');
    const cachePath=await until('DHT cache checkpoint',async()=>{
      const files=await readdir(join(dir,'receiver-data'),{recursive:true});
      for(const file of files.filter(f=>f.endsWith('/discovery.json'))) {
        const path=join(dir,'receiver-data',file), entries=JSON.parse(await readFile(path,'utf8'));
        if(entries.some(e=>!e.Hash)&&entries.some(e=>e.Hash===manifest.hash))return path;
      }
    },180,1000);
    await stopReceiver();
    const hints=JSON.parse(await readFile(cachePath,'utf8')), nodeDir=dirname(cachePath);
    manifest.results.hintCounts={dht:hints.filter(e=>!e.Hash).length,peers:hints.filter(e=>e.Hash===manifest.hash).length};
    manifest.results.discoveryTrials=[]; await save(manifest);
    // Preserve every store: move whole node directories, never delete them.
    // Each trial starts without signed state, metainfo or payloads; only hints vary.
    for(const [number,mode] of ['none','dht','both','both','dht','none'].entries()) {
      const baseline=join(dir,`baseline-${number}`);
      await rename(nodeDir,baseline); await mkdir(nodeDir,{recursive:true,mode:0o700});
      const selected=mode==='none'?[]:hints.filter(e=>mode==='both'||!e.Hash);
      await writeFile(join(nodeDir,'discovery.json'),JSON.stringify(selected),{mode:0o600});
      const began=Date.now();
      try {
        await startReceiver(magnet,false);
        const status=await until(`discovery trial ${number}`,async()=>{const s=await localStatus();return s.pid===receiver.pid&&s.sites[0].phase==='ready'&&s.sites[0].state.Core.Current===manifest.hash&&s;},360,100);
        const index=await checkRange(status.sites[0].url,source,'/index.html',0,(await stat(join(source,'index.html'))).size);
        const result={mode,elapsedMs:Date.now()-began,index,discovery:status.sites[0].state.Core.Discovery,bytes:status.sites[0].state.Core.Bytes};
        manifest.results.discoveryTrials.push(result); await save(manifest);
        console.log('Discovery trial:',JSON.stringify(result));
      } finally {
        await stopReceiver(); await rename(nodeDir,join(dir,`trial-${number}-${mode}-node`)); await rename(baseline,nodeDir);
      }
    }
    const began=Date.now();await startReceiver(magnet,false);
    const warm=await until('cached restart',async()=>{const s=await localStatus();return s.pid===receiver.pid&&s.sites[0].phase==='ready'&&s;},120,100);
    manifest.results.cachedRestart={elapsedMs:Date.now()-began,discovery:warm.sites[0].state.Core.Discovery};await save(manifest);
    console.log('Cached restart:',JSON.stringify(manifest.results.cachedRestart));
  }
  await stopReceiver();await startReceiver(magnet,true);
  const downloadStart=Date.now();const samples=[];
  const complete=await until('whole-site download',async()=>{const s=await localStatus();const c=s.sites[0].state.Core;const sample={elapsedMs:Date.now()-downloadStart,bytes:c.Bytes,total:c.Total,peers:c.Peers,error:s.sites[0].error};samples.push(sample);await writeFile(join(dir,'download-samples.json'),JSON.stringify(samples,null,2));if(Date.now()-lastReport>30000){console.log('Full download:',sample);lastReport=Date.now();}return s.event!=='stopped'&&c.Total>0&&c.Bytes===c.Total&&s;},900);
  manifest.results.fullDownload={elapsedMs:Date.now()-downloadStart,bytes:complete.sites[0].state.Core.Bytes};
  manifest.passed=manifest.results.staticBrowser.localAssetsPass&&manifest.results.normalBrowser.localAssetsPass&&manifest.results.videoRanges.every(r=>r.match)&&(manifest.results.discoveryTrials??[]).every(r=>r.index.match);
  await save(manifest);console.log('Full test result:',manifest.passed?'PASS':'FAIL (see browser evidence)');
  }
}catch(error){manifest.failure=error.message;await save(manifest);throw error;}
finally{clearTimeout(deadline);if(!hostRetained)await finish();else {await stopReceiver();console.log('Publisher retained:',JSON.stringify({ip,magnet:manifest.magnet,manifest:join(dir,'manifest.json'),hourlyCost:manifest.hourlyCost}));}}
