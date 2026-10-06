// Follow-up against an already running publisher. Owns only its local children;
// preserves all node directories and never creates/deletes cloud resources.
import {readFile,writeFile,mkdir,rename,readdir} from 'node:fs/promises';
import {resolve,join,dirname} from 'node:path';
import {spawn} from 'node:child_process';
const root=resolve(import.meta.dirname,'../..');
const dir=resolve(process.argv[2]??'');
if(!dir.startsWith(join(root,'tools/.remote-runs')+'/'))throw Error('Expected an owned remote-run directory');
const manifest=JSON.parse(await readFile(join(dir,'manifest.json'),'utf8'));
if(!manifest.keepHostRequested||!manifest.retainedAt)throw Error('Publisher must be explicitly retained');
const data=join(dir,'receiver-data');
const statusPath=join(data,'runner-status.json');
if(JSON.parse(await readFile(statusPath,'utf8')).event!=='stopped')throw Error('Original receiver must be stopped');
const files=(await readdir(data,{recursive:true})).filter(f=>f.endsWith('/discovery.json'));
if(files.length!==1)throw Error('Expected exactly one receiver node');
const nodeDir=dirname(join(data,files[0]));
const hints=JSON.parse(await readFile(join(dir,'trial-2-both-node/discovery.json'),'utf8'));
const expected=await readFile(join(root,'test_sites/blog/index.html'));
const stamp=Date.now();
const config=join(dir,`http-measure-${stamp}.yaml`);
await writeFile(config,JSON.stringify({data_dir:data,offline:false,sites:[{name:'blog',magnet:manifest.magnet,hosting:false,serve:{listen:'127.0.0.1:0'}}]}),{mode:0o600});
const pause=ms=>new Promise(r=>setTimeout(r,ms));
const results=[];
for(const [number,mode] of ['none','dht','both','both','dht','none'].entries()) {
  const baseline=join(dir,`http-${stamp}-${number}-baseline`);
  await rename(nodeDir,baseline);
  let child,exit;
  try {
    await mkdir(nodeDir,{mode:0o700});
    await writeFile(join(nodeDir,'discovery.json'),JSON.stringify(mode==='none'?[]:hints.filter(h=>mode==='both'||!h.Hash)),{mode:0o600});
    const began=performance.now();
    child=spawn(join(dir,'interweb-local'),['run','--json','--config',config],{stdio:['ignore','ignore','ignore']});
    exit=new Promise((resolve,reject)=>{child.once('exit',resolve);child.once('error',reject);});
    let origin,finished=false,lastError;
    while(performance.now()-began<120000) {
      if(child.exitCode!==null)throw Error(`Receiver exited: ${child.exitCode}`);
      try {
        if(!origin) {
          const status=JSON.parse(await readFile(statusPath,'utf8'));
          if(status.pid===child.pid)origin=status.sites[0]?.url;
        }
        if(origin) {
          const response=await fetch(new URL('/index.html',origin),{signal:AbortSignal.timeout(1000)});
          const body=Buffer.from(await response.arrayBuffer());
          if(response.status===200&&body.equals(expected)) {
            const result={mode,firstVerifiedHTTPMs:Math.round(performance.now()-began),bytes:body.length};
            results.push(result);console.log(JSON.stringify(result));
            await writeFile(join(dir,'http-discovery-results.json'),JSON.stringify(results,null,2),{mode:0o600});
            finished=true;break;
          }
        }
      }catch(error){lastError=error.message;}
      await pause(100);
    }
    if(!finished)throw Error(`HTTP discovery timeout: ${lastError??'not ready'}`);
  } finally {
    if(child) {
      child.kill('SIGTERM');const timer=setTimeout(()=>child.kill('SIGKILL'),10000);
      try{await exit;}finally{clearTimeout(timer);}
    }
    await rename(nodeDir,join(dir,`http-${stamp}-${number}-${mode}-node`));
    await rename(baseline,nodeDir);
  }
}
