import './style.css';
import './app.css';
import * as api from '../wailsjs/go/main/App.js';

document.querySelector('#app').innerHTML = `
  <aside><div class="brand">↗ interweb<span>Websites, together.</span></div>
    <nav><button data-view="hosted" class="active">Hosted sites</button><button data-view="favorites">Favorites</button><button data-view="recent">Recent sites</button></nav>
    <div class="aside-bottom"><p id="network">Connecting…</p><button id="pause">Pause hosting</button><button id="quit">Quit interweb</button></div>
  </aside>
  <main><header><div><p class="eyebrow">YOUR CORNER OF THE WEB</p><h1 id="heading">Hosted sites</h1></div><button class="primary" id="folder">+ Host a folder</button></header>
    <form id="open"><input id="magnet" aria-label="Site magnet link" placeholder="Paste a site magnet link to explore…" required><button class="primary">Open in browser ↗</button></form>
    <div id="message" role="status" hidden></div>
    <section class="toolbar"><span id="count"></span><div><button id="publish">Publish selected</button><button id="resume">Host selected</button><button id="stop">Stop selected</button></div></section>
    <section id="sites"></section>
    <details class="settings"><summary>Temporary seeding buffer</summary><p>Visited sites keep seeding until interweb exits or this buffer fills. Favorites and your hosted sites stay pinned. An evicted page can be reopened from here.</p>
      <form id="settings"><label>Sites <input id="buffer-sites" type="number" min="1" max="1000" value="10"></label><label>Buffer storage (MiB) <input id="buffer-bytes" type="number" min="16" max="1048576" value="2048"></label><label>Maximum per site (MiB) <input id="site-bytes" type="number" min="1" max="1024" value="512"></label><button>Save settings</button></form>
    </details>
  </main>`;
let state = {sites: []}, view = 'hosted', busy = false;
const selected = new Set();
const $ = (id) => document.getElementById(id);
const bytes = n => n >= 2**30 ? `${(n/2**30).toFixed(1)} GiB` : n >= 2**20 ? `${(n/2**20).toFixed(1)} MiB` : `${Math.round(n/1024)} KiB`;
function button(label, action) { const b=document.createElement('button'); b.textContent=label; b.onclick=()=>run(action); return b; }
async function refresh() { state=await api.Status(); render(); }
async function run(action) {
  if(busy) return; busy=true; $('message').hidden=false; $('message').textContent='Working…'; document.body.classList.add('busy');
  try { await action(); await refresh(); $('message').hidden=true; }
  catch(e) { $('message').textContent=String(e); }
  finally {busy=false; document.body.classList.remove('busy');}
}
function render() {
  $('network').textContent=state.network; $('pause').textContent=state.paused?'Resume hosting':'Pause hosting';
  $('heading').textContent=({hosted:'Hosted sites',favorites:'Favorites',recent:'Recent sites'})[view];
  const sites=state.sites.filter(s=>view==='hosted'?s.source:view==='favorites'?s.favorite:!s.source);
  $('count').textContent=`${sites.length} ${sites.length===1?'site':'sites'}`; $('sites').replaceChildren();
  if(!sites.length) { const empty=document.createElement('div'); empty.className='empty'; empty.textContent=view==='hosted'?'A folder. A website. Your own place on the web. Select a folder with an index.html to start hosting.':view==='favorites'?'Favorite a site to keep it available for everyone while interweb runs.':'Paste a magnet link above to explore your first site.'; $('sites').append(empty); }
  for(const s of sites) {
    const card=document.createElement('article'); card.className='site';
    const check=document.createElement('input'); check.type='checkbox'; check.checked=selected.has(s.id); check.setAttribute('aria-label',`Select ${s.name}`); check.onchange=()=>check.checked?selected.add(s.id):selected.delete(s.id);
    const body=document.createElement('div'); body.className='site-body';
    const title=document.createElement('h2'); title.textContent=s.name;
    const meta=document.createElement('p'); meta.className='muted'; meta.textContent=`${s.identity.key?'Signed identity · '+s.id.slice(0,12):'Fixed version'} · ${s.record.sequence?'Version '+s.record.sequence:'Unpublished'}`;
    const status=document.createElement('p'); status.textContent=s.status;
    const stats=document.createElement('p'); stats.className='muted'; stats.textContent=s.active?`${s.peers} peers · ${bytes(s.bytes)} / ${bytes(s.total)} · ↓ ${bytes(s.downloadRate)}/s · ↑ ${bytes(s.uploadRate)}/s`:'Not currently seeding';
    body.append(title,meta,status,stats);
    if(s.excluded?.length) { const details=document.createElement('details'); const summary=document.createElement('summary'); summary.textContent=`${s.excluded.length} excluded paths — review before publishing`; const pre=document.createElement('pre'); pre.textContent=s.excluded.join('\n'); details.append(summary,pre); body.append(details); }
    const actions=document.createElement('div'); actions.className='actions';
    actions.append(button('Open ↗',()=>api.Open(s.magnet)),button('Copy link',()=>window.runtime.ClipboardSetText(s.magnet)),button(s.favorite?'★ Favorited':'☆ Favorite',()=>api.SetFavorite(s.id,!s.favorite)));
    if(s.source) actions.append(button('Publish snapshot',()=>api.Publish(s.id)),button(s.live?'Live: on':'Live: off',()=>api.SetLive(s.id,!s.live)),button(s.hosting?'Stop hosting':'Start hosting',()=>api.SetHosting(s.id,!s.hosting)));
    else if(!s.favorite) actions.append(button('Stop seeding',()=>api.Stop(s.id)),button('Clear cached files',()=>api.ClearCache(s.id)));
    card.append(check,body,actions); $('sites').append(card);
  }
}
document.querySelectorAll('[data-view]').forEach(b=>b.onclick=()=>{view=b.dataset.view; selected.clear(); document.querySelectorAll('[data-view]').forEach(n=>n.classList.toggle('active',n===b));render();});
$('folder').onclick=()=>run(()=>api.AddFolder());
$('open').onsubmit=e=>{e.preventDefault();run(()=>api.Open($('magnet').value.trim()));};
$('publish').onclick=()=>run(async()=>{for(const id of selected)await api.Publish(id);});
$('resume').onclick=()=>run(async()=>{for(const id of selected)await api.SetHosting(id,true);});
$('stop').onclick=()=>run(async()=>{for(const id of selected){const s=state.sites.find(s=>s.id===id);await(s.source?api.SetHosting(id,false):api.Stop(id));}});
$('pause').onclick=()=>run(()=>api.SetPaused(!state.paused)); $('quit').onclick=()=>api.Quit();
$('settings').onsubmit=e=>{e.preventDefault();run(()=>api.SetSettings({bufferSites:Number($('buffer-sites').value),bufferBytes:Number($('buffer-bytes').value)*2**20,maxSiteBytes:Number($('site-bytes').value)*2**20}));};
refresh().then(()=>{$('buffer-sites').value=state.settings.bufferSites;$('buffer-bytes').value=state.settings.bufferBytes/2**20;$('site-bytes').value=(state.settings.maxSiteBytes??512*2**20)/2**20;}).catch(e=>{$('message').hidden=false;$('message').textContent=String(e);});
window.runtime?.EventsOn('sites:changed',next=>{if(!busy){state=next;render();}});
