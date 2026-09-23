'use strict';
const $ = id => document.getElementById(id);
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let token = sessionStorage.getItem('sherpa-token') || '';
// Remove fragments left in old bookmarks without using them as credentials.
if (location.hash) history.replaceState(null, '', location.pathname);
let state, current, selected = new Set(), timer;
function notice(message, problem = false) { $('notice').textContent = message; $('notice').classList.toggle('problem', problem); $('notice').hidden = !message; }
async function api(path, body) {
  const response = await fetch('/api/' + path, { method: body === undefined ? 'GET' : 'POST', headers: { Authorization: 'Bearer ' + token, ...(body === undefined ? {} : {'Content-Type':'application/json'}) }, body: body === undefined ? undefined : JSON.stringify(body) });
  if (response.status === 401) { disconnect(); }
  if (!response.ok) { let message = await response.text(); try { message = JSON.parse(message).error || message; } catch {} throw new Error(message); }
  return response;
}
async function json(path, body) { return (await api(path, body)).json(); }
function updateSelection() {
  $('selection-count').textContent = selected.size + (selected.size === 1 ? ' setup selected' : ' setups selected');
  $('run').disabled = selected.size < 2 || selected.size > 8 || (current && ['ready', 'running'].includes(current.status));
}
async function loadState() {
  state = await json('state'); state.profiles.sort((a,b) => Number(b.baseline)-Number(a.baseline) || a.name.localeCompare(b.name));
  const valid = new Set(state.profiles.map(p => p.name)); selected = new Set([...selected].filter(n => valid.has(n)));
  if (!selected.size) state.profiles.slice(0,2).forEach(p => selected.add(p.name));
  $('profiles').innerHTML = state.profiles.map(p => `<div class="profile-card ${selected.has(p.name)?'selected':''}"><label class="profile-choice"><input type="checkbox" value="${escapeHTML(p.name)}" ${selected.has(p.name)?'checked':''}><span class="profile-name">${escapeHTML(p.name)}<small class="profile-kind">${p.harness==='codex'?'CODEX':'CLAUDE CODE'}</small></span></label><div class="profile-bottom">${p.baseline?'<span class="badge">Protected baseline</span>':'<span class="badge">Variant</span>'}${state.active===p.name?'<span class="active">● Active</span>':`<button class="text-button use-profile" data-name="${escapeHTML(p.name)}">Use setup</button>`}</div><details class="profile-path"><summary>Configuration location</summary>${escapeHTML(p.path)}</details></div>`).join('');
  $('profiles').querySelectorAll('input').forEach(input => input.addEventListener('change', () => { input.checked ? selected.add(input.value) : selected.delete(input.value); input.closest('.profile-card').classList.toggle('selected',input.checked); updateSelection(); }));
  $('profiles').querySelectorAll('.use-profile').forEach(button => button.addEventListener('click', () => action({action:'use',name:button.dataset.name})));
  $('variant-from').innerHTML = state.profiles.map(p => `<option value="${escapeHTML(p.name)}">${escapeHTML(p.name)}</option>`).join('');
  $('onboarding').hidden = state.profiles.length > 0;
  $('primary').innerHTML = state.detected.map(h => `<option value="${escapeHTML(h)}">${h==='codex'?'Codex':'Claude Code'}</option>`).join('');
  $('initialize').disabled = !state.detected.length; $('no-harness').hidden = !!state.detected.length;
  $('back').disabled = !state.active;
  updateSelection();
}
async function action(body) { try { const result = await json('action',body); notice(result.message); await loadState(); } catch (error) { notice(error.message,true); } }
$('refresh').addEventListener('click', () => loadState().catch(e => notice(e.message,true)));
$('back').addEventListener('click', () => action({action:'back'}));
$('initialize').addEventListener('click', () => action({action:'init',harness:$('primary').value}));
$('create-form').addEventListener('submit', async event => {
  event.preventDefault(); const data = new FormData(event.target); const name=data.get('name');
  try { const result = await json('action',{action:'create',name,from:data.get('from'),instructions:data.get('instructions')}); selected.add(name); await loadState(); notice(result.message); event.target.reset(); } catch(error) {notice(error.message,true);}
});
$('import-form').addEventListener('submit', async event => {
  event.preventDefault(); const data = new FormData(event.target);
  try { const result = await json('action',{action:'import',name:data.get('name'),path:data.get('path'),harness:data.get('harness'),trusted:data.has('trusted')}); selected.add(data.get('name')); await loadState(); notice(result.message); event.target.reset(); } catch(error) {notice(error.message,true);}
});
$('compare-form').addEventListener('submit', async event => {
  event.preventDefault(); $('run').disabled=true; notice('Capturing the project and setups…');
  try {
    current = await json('compare',{project:$('project').value,prompt:$('prompt').value,profiles:[...selected],timeout_seconds:Number($('timeout').value)});
    notice('Comparison started. Each setup receives an independent copy.'); renderComparison(); await loadHistory(); schedule();
  } catch(error) {notice(error.message,true);updateSelection();}
});
function renderComparison() {
  const running=['ready','running'].includes(current.status);
  $('cancel').hidden=!running; $('export').hidden=running; $('rerun').hidden=running;
  const anyTruncated=current.results.some(r=>r.truncated);
  $('result-context').hidden=false; $('result-project').textContent=current.request.project; $('result-prompt').textContent=current.request.prompt;
  $('result-meta').textContent=`${current.status.replaceAll('_',' ')} · ${new Date(current.created_at).toLocaleString()} · snapshot ${current.project_hash.slice(0,12)}${anyTruncated?' · Some output was truncated':''}`;
  $('results').innerHTML=current.results.map((r,index)=>`<article class="result-card ${escapeHTML(r.status)}"><header class="result-top"><div class="result-title"><h3>${escapeHTML(r.profile)}</h3><span class="result-status">${escapeHTML(r.status.replaceAll('_',' '))}</span></div><div class="result-stats"><span>${r.harness==='codex'?'Codex':'Claude Code'}</span><span>${(r.duration_ms/1000).toFixed(1)}s</span><span>exit ${r.exit_code===-1?'—':r.exit_code}</span></div></header><div class="result-body">${r.error?`<p class="error">${escapeHTML(r.error)}</p>`:''}${r.truncated?'<p class="error">Output or diff reached the display limit.</p>':''}<pre>${escapeHTML(r.output || (r.status==='running'?'The setup is working. Results appear when this trial finishes.':r.status==='queued'?'Waiting for its turn…':'No response captured.'))}</pre><details><summary>Project changes ${r.diff?'↗':'· none'}</summary><pre class="diff">${escapeHTML(r.diff||'No file changes.')}</pre></details><details><summary>Run details & diagnostics</summary><p class="muted">${escapeHTML(r.tool_version||'Tool version unavailable')}<br>Configuration: ${escapeHTML(r.config_hash)}</p><pre>${escapeHTML(r.stderr||'No diagnostics.')}</pre></details></div><form class="rate-form" data-index="${index}"><label for="rating-${index}">YOUR RATING</label><div class="rate-controls"><select id="rating-${index}" name="score" ${running?'disabled':''}><option value="">Unrated</option>${[1,2,3,4,5].map(n=>`<option value="${n}" ${r.rating===n?'selected':''}>${n} / 5${n===5?' · Excellent':n===1?' · Poor':''}</option>`).join('')}</select><button class="text-button" type="submit" ${running?'disabled':''}>Save rating</button></div><label class="optional" for="notes-${index}">Private notes</label><textarea id="notes-${index}" name="notes" maxlength="16000" rows="2" placeholder="What worked? What would you change?" ${running?'disabled':''}>${escapeHTML(r.notes)}</textarea></form></article>`).join('');
  $('results').querySelectorAll('.rate-form').forEach(form=>form.addEventListener('submit',async event=>{
    event.preventDefault();const data=new FormData(form);const r=current.results[Number(form.dataset.index)];
    try {await json('rate',{id:current.id,profile:r.profile,score:Number(data.get('score')),notes:data.get('notes')});r.rating=Number(data.get('score'));r.notes=data.get('notes');notice(`Saved your rating for ${r.profile}.`);}catch(error){notice(error.message,true);}
  }));
  updateSelection();
}
function schedule() { clearTimeout(timer); if(current&&['ready','running'].includes(current.status)) timer=setTimeout(async()=>{ const id=current.id;try{const next=await json('comparison?id='+encodeURIComponent(id));if(current.id!==id)return;current=next;renderComparison();if(!['ready','running'].includes(current.status)){notice('Comparison finished. Review the responses and add your ratings.');await loadHistory();}schedule();}catch(error){notice(error.message,true);}},1200); }
async function loadComparison(id) { clearTimeout(timer);current=await json('comparison?id='+encodeURIComponent(id));renderComparison();schedule(); }
async function loadHistory() {
  const comparisons=await json('comparisons');$('experiment-count').textContent=String(comparisons.length+1).padStart(3,'0');
  $('history').innerHTML=comparisons.length?comparisons.map(c=>`<button class="history-item" data-id="${escapeHTML(c.id)}"><span><strong>${escapeHTML(c.request.prompt)}</strong><small>${escapeHTML(c.results.map(r=>r.profile).join(' / '))} · ${escapeHTML(c.status.replaceAll('_',' '))}</small></span><span>${new Date(c.created_at).toLocaleDateString()} ↗</span></button>`).join(''):'<p class="muted">No comparisons yet. Your experiments will stay here.</p>';
  $('history').querySelectorAll('button').forEach(button=>button.addEventListener('click',()=>loadComparison(button.dataset.id).catch(e=>notice(e.message,true))));
}
$('cancel').addEventListener('click',async()=>{try{await json('cancel',{id:current.id});notice('Stopping the comparison and saving partial results…');}catch(error){notice(error.message,true);}});
$('rerun').addEventListener('click',()=>{$('project').value=current.request.project;$('prompt').value=current.request.prompt;$('timeout').value=String(current.request.timeout_seconds);selected=new Set(current.request.profiles);loadState().catch(e=>notice(e.message,true));$('prompt').focus();notice('Prompt and lineup restored. The next comparison will capture the current project and setup files.');});
$('export').addEventListener('click',async()=>{try{const response=await api('report?id='+encodeURIComponent(current.id));const url=URL.createObjectURL(await response.blob());const a=document.createElement('a');a.href=url;a.download='sherpa-'+current.id+'.html';a.click();setTimeout(()=>URL.revokeObjectURL(url),1000);}catch(error){notice(error.message,true);}});
function disconnect() {
  clearTimeout(timer); token=''; sessionStorage.removeItem('sherpa-token');
  $('workspace').hidden=true; $('connect').hidden=false; $('access-token').focus();
}
async function connect() {
  await loadState(); await loadHistory();
  sessionStorage.setItem('sherpa-token',token);
  $('connect').hidden=true; $('workspace').hidden=false; $('access-token').value=''; notice('');
}
$('connect-form').addEventListener('submit',async event=>{
  event.preventDefault(); token=$('access-token').value.trim();
  try { await connect(); } catch(error) { notice(error.message,true); }
});
if (token) connect().catch(error=>notice(error.message,true));
