'use strict';
const $ = (s, root = document) => root.querySelector(s);
const $$ = (s, root = document) => [...root.querySelectorAll(s)];
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state, selected = new Set(), activeID = null, filter = 'all', page = 'campaign';
let editorDirty = false, formDirty = false, sendingRequest = false, currentToken = '', confirmStage = 0, pollTimer;
const validEmail = email => /^[^\s@<>,;]+@[^\s@<>,;]+\.[^\s@<>,;]+$/.test(email || '');
const selectable = lead => !['sent','sending','unknown'].includes(lead.status) && validEmail(lead.email);
const busy = () => !!(state?.sending || state?.generating);
const leadByID = id => state?.project.leads.find(l => l.id === id);
const statusText = l => ({sent:'Отправлено',error:'Ошибка',unknown:'Проверьте доставку',sending:'Отправляется'}[l.status] || (!validEmail(l.email) ? 'Нет адреса' : l.subject && l.body ? 'Черновик готов' : 'Не отправлено'));

async function request(path, body) {
  const options = body === undefined ? {} : body instanceof FormData ? {method:'POST',body} : {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)};
  const response = await fetch('/api/' + path, options);
  let data;
  try { data = await response.json(); } catch { throw new Error('Сервер недоступен. Проверьте, что приложение запущено.'); }
  if (!response.ok) throw new Error(data.error || `Ошибка ${response.status}`);
  return data;
}
let toastTimer;
function toast(message, error = false) {
  const el = $('#toast'); el.textContent = message; el.className = 'toast' + (error ? ' error' : ''); el.hidden = false;
  clearTimeout(toastTimer); toastTimer = setTimeout(() => el.hidden = true, error ? 8500 : 4500);
}
async function action(fn) { try { await fn(); } catch (error) { toast(error.message, true); } }

function visibleLeads() {
  const query = $('#search').value.toLocaleLowerCase().trim();
  return state.project.leads.filter(l => {
    const matches = filter === 'all' || filter === 'pending' && ['pending','sending'].includes(l.status) || filter === 'error' && ['error','unknown'].includes(l.status) || filter === 'sent' && l.status === 'sent';
    return matches && (!query || `${l.company} ${l.email} ${l.city}`.toLocaleLowerCase().includes(query));
  });
}

function renderList() {
  if (!state) return;
  const leads = state.project.leads, visible = visibleLeads();
  const counts = {total:leads.length,pending:leads.filter(l => ['pending','sending'].includes(l.status)).length,sent:leads.filter(l => l.status === 'sent').length,error:leads.filter(l => ['error','unknown'].includes(l.status)).length};
  for (const [key,value] of Object.entries(counts)) $(`#stat-${key}`).textContent = value;
  $('#nav-count').textContent = leads.length; $('#count-all').textContent = leads.length;
  $('#visible-count').textContent = `${visible.length} из ${leads.length}`;
  $('#lead-list').innerHTML = visible.length ? visible.map(l => `<div class="lead-row ${escapeHTML(l.status)} ${l.id === activeID ? 'active' : ''}" data-id="${escapeHTML(l.id)}" tabindex="0" role="button" aria-label="Открыть ${escapeHTML(l.company)}"><div class="row-checkbox"><input type="checkbox" aria-label="Выбрать ${escapeHTML(l.company)}" data-check="${escapeHTML(l.id)}" ${selected.has(l.id) || l.status === 'sent' ? 'checked' : ''} ${!selectable(l) || busy() ? 'disabled' : ''}></div><div class="row-content"><div class="row-top"><span class="company-name">${escapeHTML(l.company)}</span><span class="status-chip ${escapeHTML(l.status)} ${l.subject && l.status === 'pending' ? 'ready' : ''}">${statusText(l)}</span></div><div class="company-email">${escapeHTML(l.email || 'Добавьте email в редакторе →')}</div><div class="company-meta">${l.status === 'sending' ? '<span class="spinner"></span>' : ''}${escapeHTML([l.city,l.industry].filter(Boolean).join(' · ') || 'Компания')}${l.source ? `<span class="mini-source">${l.source === 'deepseek' ? '✦ DeepSeek' : 'Шаблон'}</span>` : ''}</div></div></div>`).join('') : `<div class="empty-list"><strong>${leads.length ? 'Здесь пока пусто' : 'Начните со списка компаний'}</strong>${leads.length ? 'Попробуйте другой фильтр или поиск.' : 'Загрузите CSV или перенесённый проект JSON.<br>Данные останутся на этом компьютере.'}</div>`;
  const selectableVisible = visible.filter(selectable), selectedVisible = selectableVisible.filter(l => selected.has(l.id));
  $('#select-all').checked = selectableVisible.length > 0 && selectedVisible.length === selectableVisible.length;
  $('#select-all').indeterminate = selectedVisible.length > 0 && selectedVisible.length < selectableVisible.length;
  $('#select-all').disabled = busy() || !selectableVisible.length;
  $('#selected-count').textContent = selected.size;
  $('#generate-button').disabled = !selected.size || busy() || editorDirty;
  $('#send-button').disabled = !selected.size || busy() || editorDirty || sendingRequest;
  $('#stop-button').hidden = !busy();
  $('#import-button').disabled = busy();
  $('#test-banner').hidden = !state.testRecipient;
  $('#test-banner').textContent = `Тестовый режим: отправка разрешена только на ${state.testRecipient}. Компании можно импортировать и готовить письма, но отправить на другие адреса нельзя.`;
  const banner = $('#job-banner');
  banner.hidden = !busy() && !state.jobError;
  banner.className = 'banner' + (state.jobError ? ' error' : '');
  banner.textContent = state.jobError || (state.sending ? 'Отправляем выбранные письма. Результат появится у каждой компании. Не закрывайте приложение.' : 'Готовим черновики. Компании без описания получат текст из вашего шаблона.');
}

function renderEditor() {
  const lead = leadByID(activeID); if (!lead) return;
  const locked = ['sent','sending'].includes(lead.status) || busy();
  const time = lead.sentAt ? new Date(lead.sentAt).toLocaleString('ru-RU') : '';
  $('#preview-panel').innerHTML = `<div class="preview-head"><div><div class="preview-label">ПРЕДПРОСМОТР ПИСЬМА</div><h2>${escapeHTML(lead.company)}</h2></div><span class="status-chip ${escapeHTML(lead.status)}">${statusText(lead)}</span></div><form id="editor-form" class="editor"><label>Компания<input name="company" value="${escapeHTML(lead.company)}" required ${locked ? 'disabled' : ''}></label><label>Кому<input name="email" value="${escapeHTML(lead.email)}" placeholder="name@company.ru" ${locked ? 'disabled' : ''}></label>${!validEmail(lead.email) ? '<div class="inline-note">Адрес отсутствует или некорректен. Исправьте его, чтобы выбрать компанию для отправки.</div>' : ''}${lead.error ? `<div class="inline-error">${escapeHTML(lead.error)}</div>` : ''}${lead.generationError ? `<div class="inline-error">Черновик не обновлён: ${escapeHTML(lead.generationError)}</div>` : ''}<label>Тема<input name="subject" value="${escapeHTML(lead.subject)}" placeholder="Подготовьте письмо или напишите тему" maxlength="1000" ${locked ? 'disabled' : ''}></label><label>Письмо<textarea name="body" rows="7" maxlength="50000" placeholder="Здесь появится черновик. Прочитайте и отредактируйте его перед отправкой." ${locked ? 'disabled' : ''}>${escapeHTML(lead.body)}</textarea></label><div class="editor-footer"><span id="editor-save-status">${lead.status === 'sent' ? `Принято SMTP · ${escapeHTML(time)}` : 'Изменения сохраняются кнопкой справа'}</span><button class="button secondary" type="submit" ${locked ? 'disabled' : ''}>Сохранить письмо</button></div>${lead.status === 'unknown' ? '<div class="inline-note">Проверьте доставку в почте и журналах провайдера. Если письмо точно не принято, разрешите повтор.</div><button type="button" id="resolve-button" class="button danger">Проверил: письмо не принято</button>' : ''}<details class="description"><summary>Описание компании ${lead.description ? '' : '· отсутствует'}</summary><p>${escapeHTML(lead.description || 'Для этой компании будет использован шаблон по умолчанию.')}</p></details>${lead.messageId ? `<div class="field-hint">Message-ID: ${escapeHTML(lead.messageId)}</div>` : ''}</form>`;
  editorDirty = false;
  $('#editor-form').addEventListener('input', () => { editorDirty = true; $('#editor-save-status').textContent = 'Есть несохранённые изменения'; $('#editor-save-status').className = 'dirty-note'; renderControls(); });
  $('#editor-form').addEventListener('submit', e => {e.preventDefault();action(async () => {const data = Object.fromEntries(new FormData(e.target)); await request('lead',{id:lead.id,...data}); editorDirty=false; await refresh(true); toast('Письмо сохранено');});});
  $('#resolve-button')?.addEventListener('click', () => action(async () => {if (!window.confirm('Вы проверили у почтового провайдера, что письмо не было принято? Только после этого можно повторить отправку.')) return; await request('resolve',{id:lead.id,checked:true}); await refresh(true); toast('Повторная отправка разрешена');}));
}

function renderControls() {
  $('#generate-button').disabled = !selected.size || busy() || editorDirty;
  $('#send-button').disabled = !selected.size || busy() || editorDirty || sendingRequest;
}
function fillForms() {
  for (const [key,value] of Object.entries(state.project.template)) if ($('#template-form').elements[key]) $('#template-form').elements[key].value = value;
  for (const [key,value] of Object.entries(state.settings)) if ($('#settings-form').elements[key]) $('#settings-form').elements[key].value = value;
  $('#settings-form').elements.password.value = ''; $('#settings-form').elements.apiKey.value = '';
  $('#smtp-status').textContent = state.smtpReady ? 'Данные сохранены' : 'Нужны данные';
  $('#deepseek-status').textContent = state.deepseekReady ? 'Ключ сохранён' : 'Нужен ключ';
  formDirty = false;
}
async function refresh(editor = false) {
  const wasBusy = busy(), before = leadByID(activeID);
  state = await request('state');
  selected = new Set([...selected].filter(id => {const l=leadByID(id); return l && (selectable(l) || l.status === 'sending');}));
  renderList();
  const after = leadByID(activeID);
  if (editor || !editorDirty && (wasBusy || busy() || JSON.stringify(before) !== JSON.stringify(after))) renderEditor();
  clearTimeout(pollTimer);pollTimer = setTimeout(() => action(() => refresh()),busy() ? 1500 : 5000);
}

async function navigate(next) {
  if (next === page) return;
  if ((editorDirty || formDirty) && !window.confirm('Изменения не сохранены. Перейти без сохранения?')) return;
  editorDirty=false;formDirty=false;page=next;
  $$('.page').forEach(el => el.hidden = el.id !== `page-${page}`);
  $$('.nav-button').forEach(el => el.classList.toggle('active',el.dataset.page === page));
  $('#breadcrumb').textContent = {campaign:'Рассылка',template:'Шаблон письма',settings:'Подключения'}[page];
  fillForms();if (page === 'campaign') {renderList();renderEditor();}
}

$$('.nav-button').forEach(el => el.addEventListener('click', () => navigate(el.dataset.page)));
$$('.tab').forEach(el => el.addEventListener('click', () => { filter=el.dataset.filter; $$('.tab').forEach(tab => tab.classList.toggle('active',tab === el)); renderList(); }));
$('#search').addEventListener('input',renderList);
$('#select-all').addEventListener('change', e => {for (const l of visibleLeads().filter(selectable)) {if (e.target.checked) selected.add(l.id);else selected.delete(l.id);} renderList();});
$('#lead-list').addEventListener('click', e => {
  const checkbox = e.target.closest('[data-check]');
  if (checkbox) {e.stopPropagation();if (checkbox.disabled) return; if (checkbox.checked) selected.add(checkbox.dataset.check);else selected.delete(checkbox.dataset.check);renderList();return;}
  const row = e.target.closest('[data-id]');if (!row) return;
  if (editorDirty && !window.confirm('Изменения в письме не сохранены. Открыть другую компанию?')) return;
  activeID=row.dataset.id;renderList();renderEditor();
});
$('#lead-list').addEventListener('keydown',e => {if ((e.key === 'Enter' || e.key === ' ') && e.target.matches('.lead-row')) {e.preventDefault();e.target.click();}});
$('#import-button').addEventListener('click',() => {if (editorDirty || formDirty) {toast('Сначала сохраните изменения',true);return;} $('#file-input').click();});
$('#file-input').addEventListener('change',e => action(async () => {
  const file=e.target.files[0];if (!file) return;
  try {const data=new FormData();data.append('file',file);$('#import-button').disabled=true;const result=await request('import',data);await refresh();fillForms();toast(`Добавлено: ${result.added}. Уже были в проекте: ${result.skipped}.`);} finally {e.target.value='';$('#import-button').disabled=busy();}
}));
$('#export-menu-button').addEventListener('click',()=>{const menu=$('#export-menu');menu.hidden=!menu.hidden;$('#export-menu-button').setAttribute('aria-expanded',String(!menu.hidden));});
document.addEventListener('click',e=>{if (!e.target.closest('.export-wrap')) {$('#export-menu').hidden=true;$('#export-menu-button').setAttribute('aria-expanded','false');}});
$('#generate-button').addEventListener('click',()=>action(async()=>{
  if ([...selected].some(id => leadByID(id)?.body) && !window.confirm('Заменить существующие черновики у выбранных компаний? Ручные изменения будут заменены.')) return;
  await request('generate',{ids:[...selected]});await refresh(true);
}));
$('#template-form').addEventListener('input',()=>formDirty=true);
$('#settings-form').addEventListener('input',()=>formDirty=true);
$('#template-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{await request('template',Object.fromEntries(new FormData(e.target)));formDirty=false;await refresh();fillForms();toast('Шаблон сохранён');});});
$('#settings-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{const data=Object.fromEntries(new FormData(e.target));data.port=Number(data.port);await request('settings',data);formDirty=false;await refresh();fillForms();toast('Подключения сохранены');});});
$('#smtp-test').addEventListener('click',()=>action(async()=>{
  if (formDirty) {toast('Сначала сохраните подключения',true);return;}
  const button=$('#smtp-test');button.disabled=true;$('#connection-result').textContent='Проверяем соединение и авторизацию…';
  try {const result=await request('smtp-test',{});$('#connection-result').textContent=result.message;toast('SMTP проверен, письма не отправлялись');}catch(e){$('#connection-result').textContent=e.message;throw e;}finally{button.disabled=false;}
}));

$('#send-button').addEventListener('click',()=>action(async()=>{
  if (editorDirty) throw new Error('Сначала сохраните изменения письма');
  sendingRequest=true;renderControls();
  try {
    const result=await request('prepare',{ids:[...selected]});currentToken=result.token;confirmStage=1;
    $('#confirm-step').textContent='ПОДТВЕРЖДЕНИЕ 1 ИЗ 2';$('#confirm-title').textContent='Подтвердите отправку';
    $('#confirm-text').textContent=`Вы выбрали ${result.count} писем. Отправитель: ${result.from}.`;
    $('#confirm-recipients').textContent=[...selected].map(id=>leadByID(id)).filter(Boolean).map(l=>`${l.company} → ${l.email}`).join('\n');$('#confirm-recipients').style.whiteSpace='pre-line';
    $('#confirm-note').textContent='Проверьте адресатов и тексты. На следующем шаге будет последнее подтверждение.';
    $('#confirm-next').textContent='Подтвердить';$('#confirm-dialog').showModal();
  } finally {sendingRequest=false;renderControls();}
}));
$('#confirm-next').addEventListener('click',()=>action(async()=>{
  const button=$('#confirm-next');button.disabled=true;
  try {
    if (confirmStage===1) {const result=await request('confirm',{token:currentToken});currentToken=result.token;confirmStage=2;$('#confirm-step').textContent='ПОДТВЕРЖДЕНИЕ 2 ИЗ 2';$('#confirm-title').textContent='Точно подтверждаете?';$('#confirm-note').textContent='После нажатия письма начнут уходить получателям. Отменить уже принятое сервером письмо нельзя.';button.textContent='Да, точно подтверждаю';}
    else if (confirmStage===2) {await request('send',{token:currentToken});currentToken='';confirmStage=0;$('#confirm-dialog').close();await refresh(true);toast('Отправка запущена');}
  } catch(e) {$('#confirm-dialog').close();throw e;} finally {button.disabled=false;}
}));
$('#confirm-dialog').addEventListener('close',()=>{currentToken='';confirmStage=0;});
$('#stop-button').addEventListener('click',()=>action(async()=>{await request('stop',{});toast('Очередь остановится после текущего запроса');}));
$('#quit-button').addEventListener('click',()=>action(async()=>{if (!window.confirm('Завершить работу MailDesk? Сохранённые данные останутся на компьютере.')) return;await request('quit',{});clearTimeout(pollTimer);toast('MailDesk завершён. Для продолжения запустите приложение снова.');$$('button').forEach(b=>b.disabled=true);}));
window.addEventListener('beforeunload',e=>{if (editorDirty||formDirty) {e.preventDefault();e.returnValue='';}});
action(async()=>{await refresh();fillForms();});
