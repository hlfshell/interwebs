import assert from 'node:assert/strict';
import {spawn, execFile} from 'node:child_process';
import {once} from 'node:events';
import {mkdtemp, mkdir, writeFile, readFile, rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join, resolve, dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createInterface} from 'node:readline';
import {promisify} from 'node:util';
import {createServer} from 'node:http';
import {chromium, firefox} from 'playwright';

const exec = promisify(execFile);
const core = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const temporary = await mkdtemp(join(tmpdir(), 'interweb-core-browser-'));
try {
  const binary = join(temporary, 'fixture');
  await exec('go', ['build', '-buildvcs=false', '-o', binary, './internal/browserfixture'],
    {cwd:core, env:{...process.env, GOWORK:'off'}});
  for (const name of (process.env.BROWSERS || 'chromium,firefox').split(',')) {
    const site = join(temporary, name, 'source');
    const state = join(temporary, name, 'node');
    await mkdir(site, {recursive:true});
    await mkdir(state);
    let externalRequests = 0;
    const external = createServer((req,res) => { externalRequests++; res.end('outside'); });
    await new Promise(resolve => external.listen(0, '127.0.0.1', resolve));
    const outside = 'http://127.0.0.1:' + external.address().port;
    await exec('ffmpeg', ['-hide_banner','-loglevel','error','-f','lavfi','-i',
      'color=c=green:s=160x120:r=15','-t','3','-c:v','libvpx','-an',join(site,'movie.webm')]);
    await exec('ffmpeg', ['-hide_banner','-loglevel','error','-f','lavfi','-i',
      'sine=frequency=440:duration=3',join(site,'tone.wav')]);
    await writeFile(join(site,'index.html'), `<!doctype html><link rel="stylesheet" href="style.css">
      <h1>Stored site</h1><a href="next.html">Next</a><video id="video" muted src="movie.webm"></video>
      <audio id="audio" muted src="tone.wav"></audio><script type="module" src="app.js"></script>`);
    await writeFile(join(site,'next.html'), '<h1>Second page</h1>');
    await writeFile(join(site,'style.css'), 'h1 { color: rgb(10, 90, 30) }');
    await writeFile(join(site,'data.json'), '{"answer":42}');
    await writeFile(join(site,'module.js'), 'export const value=7;');
    await writeFile(join(site,'app.js'), `import {value} from './module.js';
      window.checks={module:value,data:(await fetch('./data.json').then(r=>r.json())).answer};
      try {localStorage.setItem('x','x');checks.storage=true;}catch{checks.storage=false;}
      try {await navigator.serviceWorker.register('./app.js');checks.worker=true;}catch{checks.worker=false;}
      try {await fetch('${outside}/escape');checks.external=true;}catch{checks.external=false;}
      checks.native=!!window.go;checks.done=true;`);
    const child = spawn(binary,[site,state],{stdio:['pipe','pipe','inherit']});
    const exited = once(child,'exit');
    const lines = createInterface({input:child.stdout})[Symbol.asyncIterator]();
    const next = async () => {
      const result = await lines.next();
      if(result.done) throw new Error('fixture exited before responding');
      return JSON.parse(result.value);
    };
    let browser;
    try {
      const {url,hash} = await next();
      const manifest = await readFile(join(state,'data',hash,'manifest'));
      assert.equal(manifest.includes(Buffer.from('index.html')),false);
      const engine = name === 'chromium' ? chromium : firefox;
      const executablePath = name === 'chromium' ? process.env.CHROMIUM_PATH : process.env.FIREFOX_PATH;
      browser = await engine.launch({headless:true,...(executablePath?{executablePath}:{})});
      const page = await browser.newPage();
      await page.goto(url);
      await page.waitForFunction(()=>window.checks?.done);
      assert.deepEqual(await page.evaluate(()=>window.checks),
        {module:7,data:42,storage:false,worker:false,external:false,native:false,done:true});
      assert.equal(await page.locator('h1').evaluate(e=>getComputedStyle(e).color),'rgb(10, 90, 30)');
      for (const selector of ['#video','#audio']) {
        await page.locator(selector).evaluate(async e=>{await e.play();});
        await page.waitForFunction(s=>document.querySelector(s).currentTime>0,selector);
        await page.locator(selector).evaluate(e=>{e.pause();e.currentTime=1.5;});
        await page.waitForFunction(s=>document.querySelector(s).currentTime>=1.5,selector);
      }
      child.stdin.write('stop\n');
      assert.equal((await next()).Seeding,false);
      await page.getByText('Next',{exact:true}).click();
      assert.equal(await page.locator('h1').textContent(),'Second page');
      const range = await page.request.get(url+'movie.webm',{headers:{Range:'bytes=0-31'}});
      assert.equal(range.status(),206);
      assert.equal((await range.body()).length,32);
      await page.close();
      child.stdin.write('seed\n');
      assert.equal((await next()).Seeding,true);
      assert.equal(externalRequests,0);
      console.log(name+': encrypted viewing, modules, media, seeking, navigation, Stop/Seed passed');
    } finally {
      await browser?.close();
      child.stdin.end();
      const timeout = setTimeout(()=>child.kill('SIGKILL'),5000);
      await exited;
      clearTimeout(timeout);
      external.close();
    }
  }
} finally {
  await rm(temporary,{recursive:true,force:true});
}
