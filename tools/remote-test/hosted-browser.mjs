import {readFile,open} from 'node:fs/promises';
import {resolve} from 'node:path';
import {createHash} from 'node:crypto';
import {chromium} from '../../interwebs/browser/node_modules/playwright/index.mjs';

const digest=b=>createHash('sha256').update(b).digest('hex');

// Exercise the actual receiving HTTP origin, not a static-file substitute.
export async function checkBrowser(origin, source, screenshot, {blockVideo=false}={}) {
  const browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH||'/etc/profiles/per-user/hlfshell/bin/chromium'});
  const started=Date.now(),requests=[],failures=[],errors=[],checks=[];
  try {
    const page=await browser.newPage({viewport:{width:1440,height:1000}});
    if(blockVideo)await page.route('**/*.mp4',route=>route.abort());
    page.on('pageerror',e=>errors.push(e.message));
    page.on('requestfailed',r=>failures.push({url:r.url(),error:r.failure()?.errorText}));
    page.on('response',response=>{
      if(!response.url().startsWith(origin))return;
      const url=new URL(response.url());
      requests.push({path:url.pathname,status:response.status()});
      if(url.pathname.endsWith('.mp4'))return;
      checks.push((async()=>{
        try {
          const body=await response.body();
          const path=resolve(source,'.'+decodeURIComponent(url.pathname)+(url.pathname.endsWith('/')?'index.html':''));
          if(!path.startsWith(resolve(source)+'/'))throw Error('path escaped source');
          const expected=await readFile(path);
          return {path:url.pathname,bytes:body.length,match:body.equals(expected),sha256:digest(body)};
        }catch(e){return {path:url.pathname,error:e.message};}
      })());
    });
    let loadError;
    try {await page.goto(origin,{waitUntil:'load',timeout:90000});}catch(e){loadError=e.message;}
    const loadMs=Date.now()-started;
    await page.waitForTimeout(2000);
    const images=await page.locator('img').evaluateAll(nodes=>nodes.map(i=>({src:i.getAttribute('src'),loaded:i.complete&&i.naturalWidth>0})));
    const video=await page.locator('video').evaluateAll(nodes=>nodes.map(v=>({paused:v.paused,time:v.currentTime,ready:v.readyState,preload:v.preload})));
    await page.screenshot({path:screenshot,timeout:15000});
    const verified=await Promise.all(checks);
    return {blockVideo,loadMs,loadError,title:await page.title(),images,video,requests,verified,failures,errors,
      localAssetsPass:!loadError&&images.every(i=>i.loaded)&&verified.length>0&&verified.every(v=>v.match)};
  }finally{await browser.close();}
}

export async function checkRange(origin, source, path, start, length) {
  const began=Date.now();
  const response=await fetch(new URL(path,origin),{headers:{Range:`bytes=${start}-${start+length-1}`},signal:AbortSignal.timeout(90000)});
  const body=Buffer.from(await response.arrayBuffer());
  const file=await open(resolve(source,'.'+path));
  const expected=Buffer.alloc(length);
  try{await file.read(expected,0,length,start);}finally{await file.close();}
  return {path,start,length,status:response.status,bytes:body.length,elapsedMs:Date.now()-began,match:response.status===206&&body.equals(expected)};
}
