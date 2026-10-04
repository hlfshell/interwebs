// Public-network acceptance runner. All billable resources are tagged and
// recorded before the next operation. No DigitalOcean credential leaves this host.
import {spawn, execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {mkdir, readFile, writeFile, readdir, rename} from 'node:fs/promises';
import {resolve, join, dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {randomUUID} from 'node:crypto';
import {createInterface} from 'node:readline';
import {cleanupOnce,finalizeRun,cleanupResources,provisioningGuard} from './remote-lifecycle.mjs';

const exec = promisify(execFile);
const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const runs = join(root, '.remote-runs');
const pause = ms => new Promise(r => setTimeout(r, ms));
const command = async (name, args, options={}) => (await exec(name,args,{cwd:root,timeout:60000,maxBuffer:8<<20,...options})).stdout;
const doctl = async (...args) => JSON.parse(await command('doctl',[...args,'--output','json']));
await mkdir(runs,{recursive:true,mode:0o700});
async function save(m) {const p=join(runs,m.tag,'manifest.json');await writeFile(p+'.tmp',JSON.stringify(m,null,2),{mode:0o600});await rename(p+'.tmp',p);}
async function cleanup(m) {
  console.log(`Cleaning ${m.tag}`);
  await cleanupResources(m,{doctl,command,pause,save,log:console.log});
}
for(const entry of await readdir(runs,{withFileTypes:true})) {
  if(!entry.isDirectory()||!/^interweb-test-[0-9]+-[a-f0-9-]+$/.test(entry.name))continue;
  const m=JSON.parse(await readFile(join(runs,entry.name,'manifest.json'),'utf8'));
  if(m.tag!==entry.name)throw Error('Invalid resource manifest');
  if(!m.cleanedAt && (Date.now()>m.expiresAt || process.argv.includes('--cleanup-current')))await cleanup(m);
}
// Discover orphaned tagged droplets even if a previous process lost its manifest.
for(const d of await doctl('compute','droplet','list')) {
  const tag=d.tags?.find(t=>/^interweb-test-[0-9]+-[a-f0-9-]+$/.test(t));
  if(tag && Date.now()>Number(tag.split('-')[2])+30*60*1000) {
    await mkdir(join(runs,tag),{recursive:true,mode:0o700});
    await cleanup({tag,expiresAt:0,reconciled:true});
  }
}
for(const item of [...await doctl('compute','tag','list'),...await doctl('compute','ssh-key','list')]) {
  const tag=item.name;
  if(/^interweb-test-[0-9]+-[a-f0-9-]+$/.test(tag) && Date.now()>Number(tag.split('-')[2])+30*60*1000) {
    await mkdir(join(runs,tag),{recursive:true,mode:0o700});await cleanup({tag,expiresAt:0,reconciled:true});
  }
}
if(process.argv.includes('--cleanup') || process.argv.includes('--cleanup-current'))process.exit(0);

const tag=`interweb-test-${Date.now()}-${randomUUID()}`;
const dir=join(runs,tag);await mkdir(dir,{mode:0o700});
const manifest={tag,expiresAt:Date.now()+30*60*1000,droplets:[],keyID:null};await save(manifest);
const nodes=[];
const provisioning=provisioningGuard();
const finish=cleanupOnce(async()=>{await provisioning.drain();for(const n of nodes){try{n.close();}catch{}}await cleanup(manifest);});
for(const signal of ['SIGINT','SIGTERM'])process.once(signal,()=>{finish().then(()=>process.exit(130)).catch(e=>{console.error(e);process.exit(1);});});
const deadline=setTimeout(()=>{console.error('Remote test reached 30-minute deadline');finish().then(()=>process.exit(1)).catch(e=>{console.error(e);process.exit(1);});},30*60*1000);

class Node {
  constructor(ip) {
    if(provisioning.closing)throw Error('Run is shutting down');
    this.ip=ip;this.pending=[];
    this.process=spawn('ssh',[...sshOptions,`root@${ip}`,'/root/interweb-node -data /root/interweb-data -key-file /root/interweb-storage.key -port 42069 -update-interval 3s -network-interval 1s -announce-interval 10s'],{stdio:['pipe','pipe','pipe']});
    this.exited=new Promise(resolve=>this.process.once('exit',resolve));
    this.log='';this.process.stderr.on('data',b=>{this.log+=b;});
    createInterface({input:this.process.stdout}).on('line',line=>{try{const result=JSON.parse(line);const p=this.pending.shift();if(p){clearTimeout(p.timer);result.error?p.reject(Error(result.error)):p.resolve(result.result);}}catch{this.log+=line+'\n';}});
    this.process.on('exit',()=>{for(const p of this.pending){clearTimeout(p.timer);p.reject(Error('Remote node exited'));}this.pending=[];});
  }
  call(op,fields={}) {return new Promise((resolve,reject)=>{const p={resolve,reject,timer:setTimeout(()=>{reject(Error(`Node ${op} timed out`));this.process.kill();},180000)};this.pending.push(p);this.process.stdin.write(JSON.stringify({op,...fields})+'\n');});}
  close(){if(!this.process.stdin.writableEnded)this.process.stdin.end();return this.exited;}
}
const sshOptions=['-i',join(dir,'key'),'-o','BatchMode=yes','-o','ConnectTimeout=10','-o','StrictHostKeyChecking=accept-new','-o',`UserKnownHostsFile=${join(dir,'known_hosts')}`];
async function ssh(ip,args){return command('ssh',[...sshOptions,`root@${ip}`,...args]);}
async function until(label,fn,seconds=300){const end=Date.now()+seconds*1000;let last;while(Date.now()<end){try{const r=await fn();if(r)return r;}catch(e){last=e;}await pause(5000);}throw Error(`${label} timed out${last?': '+last.message:''}`);}

try {
  await command('go',['build','-buildvcs=false','-o',join(dir,'interweb-node'),'./cmd/interweb-node'],{env:{...process.env,GOOS:'linux',GOARCH:'amd64',CGO_ENABLED:'0'},timeout:180000});
  const sizes=await doctl('compute','size','list');
  const size=sizes.filter(s=>s.available&&s.description==='Basic'&&s.memory>=1024&&s.regions?.includes('nyc3')&&s.regions?.includes('sfo3')).sort((a,b)=>a.price_hourly-b.price_hourly)[0];
  if(!size || size.price_hourly*3>1)throw Error('No suitable configuration below the $1 run budget');
  console.log(`Using 3 × ${size.slug} ($${size.price_hourly}/hour each), 30-minute deadline`);
  manifest.size=size.slug;manifest.hourlyCost=size.price_hourly*3;await save(manifest);
  await command('ssh-keygen',['-t','ed25519','-N','','-f',join(dir,'key'),'-C',tag]);
  const imported=await provisioning.track(()=>doctl('compute','ssh-key','import',tag,'--public-key-file',join(dir,'key.pub')));manifest.keyID=imported[0].id;await save(manifest);
  for(const [i,region]of ['nyc3','sfo3','nyc3'].entries()) {
    const created=await provisioning.track(()=>doctl('compute','droplet','create',`${tag}-${i}`,'--size',size.slug,'--image','ubuntu-24-04-x64','--region',region,'--ssh-keys',String(manifest.keyID),'--tag-name',tag));
    manifest.droplets.push({id:created[0].id,region});await save(manifest);
  }
  for(const resource of manifest.droplets) {
    const ip=await until('droplet readiness',async()=>{const [d]=await doctl('compute','droplet','get',String(resource.id));return d.status==='active'&&d.networks.v4.find(n=>n.type==='public')?.ip_address;});
    resource.ip=ip;await save(manifest);
    await until('SSH readiness',()=>ssh(ip,['true']).then(()=>true));
    await command('scp',[...sshOptions,join(dir,'interweb-node'),`root@${ip}:/root/interweb-node`]);
    const node=new Node(ip);nodes.push(node);await node.call('status');
  }
  let [publisher,replica,reader]=nodes;
  // Fixed literal remote commands only; site identities travel over the JSON interface.
  await ssh(publisher.ip,["mkdir -p /root/site-a /root/site-b && printf '<h1>alpha version one</h1>' > /root/site-a/index.html && printf '<h1>beta independent identity</h1>' > /root/site-b/index.html"]);
  await ssh(publisher.ip,['dd if=/dev/urandom of=/root/site-a/movie.mp4 bs=1048576 count=8 status=none']);
  const a=await publisher.call('add',{path:'/root/site-a'}),b=await publisher.call('add',{path:'/root/site-b'});
  if(a===b)throw Error('Publisher identities collided');
  await publisher.call('publish',{id:a});await publisher.call('publish',{id:b});
  await until('signed DHT publication',async()=>{const s=await publisher.call('status');return s.sites.length===2&&s.sites.every(x=>x.status.includes('announced'));});
  const published=(await publisher.call('status')).sites;
  for(const s of published)await until('public-DHT site discovery',()=>replica.call('open',{magnet:s.magnet}));
  const partial=(await replica.call('status')).sites.find(s=>s.id===a);if(partial.bytes>=partial.total)throw Error('Browsing downloaded the unrelated movie');
  await replica.call('favorite',{id:a,enabled:true});await replica.call('favorite',{id:b,enabled:true});
  await ssh(publisher.ip,["printf '<h1>alpha version two</h1>' > /root/site-a/index.html"]);await publisher.call('publish',{id:a});
  await until('updated record announcement',async()=>{const s=(await publisher.call('status')).sites.find(s=>s.id===a);return s.record.sequence===2&&s.status.includes('announced');});
  await until('automatic signed update discovery',async()=>{const s=(await replica.call('status')).sites.find(s=>s.id===a);return s.record.sequence===2&&s.current===s.record.hash;});
  await until('favorite completion',async()=>{const s=await replica.call('status');return s.sites.every(x=>x.bytes===x.total);});
  publisher.close();await ssh(publisher.ip,['pkill -TERM -x interweb-node || true']);
  await replica.close();await writeFile(join(dir,'replica-before-restart.log'),replica.log,{mode:0o600});replica=new Node(replica.ip);nodes[1]=replica;
  await until('favorite restart',async()=>{const s=await replica.call('status');return s.sites.length===2&&s.sites.every(x=>x.favorite&&x.active&&x.bytes===x.total);});
  const address=await until('fresh reader from replica',()=>reader.call('open',{magnet:published[0].magnet}));
  if(!/^http:\/\/127\.0\.0\.1:\d+\/$/.test(address))throw Error('Unexpected local URL');
  const html=await ssh(reader.ip,['curl','--fail','--silent',address]);if(!html.includes('alpha version two'))throw Error('Reader did not receive updated content');
  console.log('PASS: two identities, selective download, automatic signed update, favorite restart, publisher-offline fresh reader');
  manifest.passed=true;await save(manifest);
} finally {
  clearTimeout(deadline);
  await finalizeRun(async()=>{for(let i=0;i<nodes.length;i++)await writeFile(join(dir,`node-${i}.log`),nodes[i].log,{mode:0o600});},finish);
}
