import {test,expect} from '@playwright/test';
import {spawn,execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {createInterface} from 'node:readline';
import {mkdtemp,mkdir,writeFile,rm,readdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join,resolve} from 'node:path';
import {createServer} from 'node:http';

test('direct browser pages execute bundled modules but cannot escape the sandbox',async({page})=>{
  page.on('pageerror',error=>console.error('Site script:',error.message));
  page.on('console',message=>{if(message.type()==='error')console.error('Site console:',message.text());});
  const dir=await mkdtemp(join(tmpdir(),'interweb-browser-'));const source=join(dir,'site');await mkdir(source);
  let externalRequests=0;const external=createServer((req,res)=>{externalRequests++;res.setHeader('Access-Control-Allow-Origin','*');res.end('outside');});
  await new Promise(r=>external.listen(0,'127.0.0.1',r));const outside=`http://127.0.0.1:${external.address().port}`;
  await promisify(execFile)('ffmpeg',['-hide_banner','-loglevel','error','-f','lavfi','-i','color=c=green:s=160x120:r=15','-t','3','-c:v','libvpx','-an',join(source,'movie.webm')]);
  await promisify(execFile)('ffmpeg',['-hide_banner','-loglevel','error','-f','lavfi','-i','sine=frequency=440:duration=3',join(source,'tone.wav')]);
  await writeFile(join(source,'index.html'),`<!doctype html><link rel="stylesheet" href="style.css"><h1>Direct site</h1><video id="video" muted src="movie.webm"></video><audio id="audio" muted src="tone.wav"></audio><form action="${outside}/form"><button id="submit">Submit</button></form><script type="module" src="app.js"></script>`);
  await writeFile(join(source,'style.css'),'h1 { color: rgb(10, 90, 30) }');
  await writeFile(join(source,'data.json'),' {"answer":42} ');
  await writeFile(join(source,'app.js'),`import {value} from './module.js';window.checks={module:value};
    checks.data=(await fetch('./data.json').then(r=>r.json())).answer;
    try {localStorage.setItem('escape','yes');checks.storage=true;}catch{checks.storage=false;}
    try {await navigator.serviceWorker.register('./app.js');checks.worker=true;}catch{checks.worker=false;}
    try {await fetch('${outside}/escape');checks.external=true;}catch{checks.external=false;}
    try {checks.popup=window.open('${outside}/popup')!==null;}catch{checks.popup=false;}
    try {await fetch('__SIBLING__');checks.sibling=true;}catch{checks.sibling=false;}
    checks.native=!!window.go;checks.done=true;`);
  await writeFile(join(source,'module.js'),'export const value = 7;');
  const child=spawn(resolve('../build/bin/interweb-node'),['-offline','-data',join(dir,'state')],{stdio:['pipe','pipe','pipe']});
  const pending=[];child.stderr.on('data',()=>{});createInterface({input:child.stdout}).on('line',line=>{const result=JSON.parse(line);const p=pending.shift();result.error?p.reject(Error(result.error)):p.resolve(result.result);});
  const call=(op,fields={})=>new Promise((resolve,reject)=>{pending.push({resolve,reject});child.stdin.write(JSON.stringify({op,...fields})+'\n');});
  try {
    const sibling=join(dir,'sibling');await mkdir(sibling);await writeFile(join(sibling,'index.html'),'sibling content');const siblingID=await call('add',{path:sibling});await call('publish',{id:siblingID});const siblingURL=(await call('status')).sites.find(s=>s.id===siblingID).url;
    const {readFile}=await import('node:fs/promises');const script=await readFile(join(source,'app.js'),'utf8');await writeFile(join(source,'app.js'),script.replace('__SIBLING__',siblingURL));
    const id=await call('add',{path:source});await call('publish',{id});const status=await call('status');const url=await call('open',{magnet:status.sites.find(s=>s.id===id).magnet});
    const stored=join(dir,'state','data',status.sites.find(s=>s.id===id).current);
    expect((await readdir(stored)).sort()).toEqual(['chunks','lock','manifest']);
    expect((await readFile(join(stored,'manifest'))).includes(Buffer.from('index.html'))).toBe(false);
    await page.goto(url);await page.waitForFunction(()=>window.checks?.done);
    expect(await page.evaluate(()=>window.checks)).toEqual({module:7,data:42,storage:false,worker:false,external:false,popup:false,sibling:false,native:false,done:true});
    await expect(page.locator('h1')).toHaveCSS('color','rgb(10, 90, 30)');expect(externalRequests).toBe(0);
    expect(await page.locator('iframe').count()).toBe(0);
    await page.click('#submit');await expect(page.locator('h1')).toHaveText('Direct site');expect(externalRequests).toBe(0);
    for(const selector of ['#video','#audio']){
      await page.locator(selector).evaluate(async e=>{await e.play();});
      await expect.poll(()=>page.locator(selector).evaluate(e=>e.currentTime)).toBeGreaterThan(0);
      await page.locator(selector).evaluate(e=>{e.pause();e.currentTime=1.5;});
      await expect.poll(()=>page.locator(selector).evaluate(e=>e.currentTime)).toBeGreaterThan(1);
    }
    await page.screenshot({path:`test-results/encrypted-site-${test.info().project.name}.png`,fullPage:true});
    await page.close();expect((await call('status')).sites.find(s=>s.id===id).active).toBe(true);
  } finally {child.stdin.end();await new Promise(r=>child.once('exit',r));external.close();await rm(dir,{recursive:true,force:true});}
});

test('desktop UI exposes independent sites and escapes remote names',async({page})=>{
  await page.addInitScript(()=>{
    const sites=[1,2].map(n=>({id:String(n),name:n===1?'<img src=x onerror=alert(1)>':'Second site',source:'/website/'+n,identity:{key:'abc'},record:{sequence:n},magnet:'magnet:?xs=urn:btpk:'+n,status:'Ready',active:false,excluded:[],favorite:false,hosting:true,live:false}));
    window.calls=[];window.go={main:{App:new Proxy({Status:async()=>({sites,network:'Public DHT enabled',settings:{bufferSites:10,bufferBytes:2147483648},paused:false})},{get:(target,key)=>target[key]||((...args)=>{window.calls.push([key,...args]);return Promise.resolve();})})}};
    window.runtime={EventsOn(){},ClipboardSetText(){}};
  });
  await page.goto('http://127.0.0.1:4173');await expect(page.locator('article')).toHaveCount(2);await expect(page.locator('article img')).toHaveCount(0);
  await page.getByRole('checkbox').first().check();await page.getByRole('checkbox').last().check();await page.getByRole('button',{name:'Publish selected',exact:true}).click();
  await expect.poll(()=>page.evaluate(()=>window.calls)).toEqual([['Publish','1'],['Publish','2']]);
  await page.locator('summary').click();
  await expect(page.locator('#site-bytes')).toHaveValue('512');
  await page.locator('#site-bytes').fill('256');
  await page.getByRole('button',{name:'Save settings',exact:true}).click();
  await expect.poll(()=>page.evaluate(()=>window.calls.at(-1))).toEqual(['SetSettings',{bufferSites:10,bufferBytes:2147483648,maxSiteBytes:268435456}]);
  await page.screenshot({path:'test-results/desktop.png',fullPage:true});
});
