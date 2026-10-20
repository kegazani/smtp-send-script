'use strict';
const $ = (s, root = document) => root.querySelector(s);
const $$ = (s, root = document) => [...root.querySelectorAll(s)];
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let state, selected = new Set(), activeID = null, filter = 'all', page = 'campaign';
let editorDirty = false, formDirty = false, sendingRequest = false, uploading = false, restoring = false;
let currentToken = '', confirmStage = 0, pollTimer, toastTimer, composeFiles = [];
const validEmail = email => /^[^\s@<>,;]+@[^\s@<>,;]+\.[^\s@<>,;]+$/.test(email || '');
const addresses = lead => lead.kind === 'message' ? lead.recipients || [] : [lead.email];
const recipientText = lead => addresses(lead).join(', ');
const validDestination = lead => addresses(lead).length > 0 && addresses(lead).every(validEmail);
const selectable = lead => !['sent','sending','unknown'].includes(lead.status) && validDestination(lead);
const busy = () => !!(state?.sending || state?.generating || uploading || restoring);
const leadByID = id => state?.project.leads.find(l => l.id === id);
const selectedLeads = () => [...selected].map(leadByID).filter(Boolean);
const statusText = l => ({sent:'Отправлено',error:'Ошибка',unknown:'Проверьте доставку',sending:'Отправляется'}[l.status] || (l.generationError ? 'Ошибка генерации' : !validDestination(l) ? 'Нет адреса' : l.subject && l.body ? 'Черновик готов' : 'Не отправлено'));
const sourceText = l => ({deepseek:'✦ DeepSeek',template:'Шаблон · нет описания',manual:'Вручную'}[l.source] || '');
const splitRecipients = value => value.split(/[,;\s]+/).filter(Boolean);
const sizeText = size => size >= 1048576 ? `${(size / 1048576).toFixed(1)} МБ` : `${Math.max(1,Math.ceil(size / 1024))} КБ`;

async function request(path, body) {
  const options = body === undefined ? {} : body instanceof FormData ? {method:'POST',body} : {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)};
  const response = await fetch('/api/' + path, options);
  let data;
  try { data = await response.json(); } catch { throw new Error('Сервер недоступен. Проверьте, что приложение запущено.'); }
  if (!response.ok) throw new Error(data.error || `Ошибка ${response.status}`);
  return data;
}
function toast(message, error = false) {
  const el = $('#toast'); el.textContent = message; el.className = 'toast' + (error ? ' error' : ''); el.hidden = false;
  clearTimeout(toastTimer); toastTimer = setTimeout(() => el.hidden = true, error ? 8500 : 4500);
}
async function action(fn) { try { await fn(); } catch (error) { toast(error.message, true); } }
function checkFiles(files) {
  if (files.length > 20) throw new Error('Не больше 20 файлов в письме');
  if (files.some(f => f.size > 10 * 1048576)) throw new Error('Один файл не должен превышать 10 МБ');
  if (files.reduce((sum,f) => sum + f.size,0) > 15 * 1048576) throw new Error('Суммарный размер вложений не должен превышать 15 МБ');
}
function visibleLeads() {
  const query = $('#search').value.toLocaleLowerCase().trim();
  return state.project.leads.filter(l => {
    const matches = filter === 'all' || filter === 'pending' && ['pending','sending'].includes(l.status) || filter === 'error' && ['error','unknown'].includes(l.status) || filter === 'sent' && l.status === 'sent';
    return matches && (!query || `${l.company} ${recipientText(l)} ${l.subject} ${l.city}`.toLocaleLowerCase().includes(query));
  });
}
function renderControls() {
  $('#generate-button').disabled = !selectedLeads().some(l => l.kind !== 'message') || busy() || editorDirty;
  $('#send-button').disabled = !selected.size || busy() || editorDirty || sendingRequest;
  $('#bulk-attach-button').disabled = !selected.size || busy() || editorDirty;
  $$('.editor .attachment-mutation').forEach(button => button.disabled = editorDirty || busy());
  $$('[data-restore]').forEach(button => button.disabled = editorDirty || busy());
  $$('#compose-form button').forEach(button => button.disabled = busy());
  $('#stop-button').hidden = !state?.sending && !state?.generating;
}
function renderList() {
  if (!state) return;
  const leads = state.project.leads, visible = visibleLeads();
  const counts = {total:leads.length,pending:leads.filter(l => ['pending','sending'].includes(l.status)).length,sent:leads.filter(l => l.status === 'sent').length,error:leads.filter(l => ['error','unknown'].includes(l.status)).length};
  for (const [key,value] of Object.entries(counts)) $(`#stat-${key}`).textContent = value;
  $('#nav-count').textContent = leads.length; $('#count-all').textContent = leads.length;
  $('#visible-count').textContent = `${visible.length} из ${leads.length}`;
  $('#lead-list').innerHTML = visible.length ? visible.map(l => `
    <div class="lead-row ${escapeHTML(l.status)} ${l.id === activeID ? 'active' : ''}" data-id="${escapeHTML(l.id)}" tabindex="0" role="button" aria-label="Открыть ${escapeHTML(l.company)}">
      <div class="row-checkbox"><input type="checkbox" aria-label="Выбрать ${escapeHTML(l.company)}" data-check="${escapeHTML(l.id)}" ${selected.has(l.id) || l.status === 'sent' ? 'checked' : ''} ${!selectable(l) || busy() ? 'disabled' : ''}></div>
      <div class="row-content"><div class="row-top"><span class="company-name">${escapeHTML(l.company)}</span><span class="status-chip ${escapeHTML(l.status)} ${l.generationError ? 'error' : l.subject && l.body && l.status === 'pending' ? 'ready' : ''}">${statusText(l)}</span></div>
      <div class="company-email">${escapeHTML(recipientText(l) || 'Добавьте email в редакторе →')}</div>
      <div class="company-meta">${l.status === 'sending' ? '<span class="spinner"></span>' : ''}${escapeHTML(l.kind === 'message' ? `Обычное письмо · ${addresses(l).length} получ.` : [l.city,l.industry].filter(Boolean).join(' · ') || 'Компания')}${l.source ? `<span class="mini-source">${sourceText(l)}</span>` : ''}${l.attachments?.length ? `<span class="mini-source">⌕ ${l.attachments.length}</span>` : ''}</div></div>
      ${l.status === 'sent' ? `<button type="button" class="row-restore" data-restore="${escapeHTML(l.id)}" title="Вернуть в черновики для повторной отправки" aria-label="Вернуть в черновики: ${escapeHTML(l.company)}" ${busy() || editorDirty ? 'disabled' : ''}><svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 10a9 9 0 1 1 2.6 8.4M3 4v6h6"/></svg></button>` : ''}
    </div>`).join('') : `<div class="empty-list"><strong>${leads.length ? 'Здесь пока пусто' : 'Начните с первого письма'}</strong>${leads.length ? 'Попробуйте другой фильтр или поиск.' : 'Напишите письмо или загрузите CSV компаний.<br>Данные останутся на этом компьютере.'}</div>`;
  const selectableVisible = visible.filter(selectable), selectedVisible = selectableVisible.filter(l => selected.has(l.id));
  $('#select-all').checked = selectableVisible.length > 0 && selectedVisible.length === selectableVisible.length;
  $('#select-all').indeterminate = selectedVisible.length > 0 && selectedVisible.length < selectableVisible.length;
  $('#select-all').disabled = busy() || !selectableVisible.length;
  $('#selected-count').textContent = selected.size;
  $('#import-button').disabled = busy();
  $('#test-banner').hidden = !state.testRecipient;
  $('#test-banner').textContent = `Тестовый режим: отправка разрешена только на ${state.testRecipient}.`;
  const g=state.generation;
  const summary=g?.completed ? `Последняя подготовка: ${g.completed} из ${g.total}. DeepSeek: ${g.personalized}; шаблон без описания: ${g.templates}; ошибки: ${g.failed}.${g.failed ? ' Предыдущий текст сохранён. '+g.error : ''}` : '';
  const banner = $('#job-banner'); banner.hidden = !busy() && !state.jobError && !summary;
  banner.className = 'banner' + (state.jobError || !busy() && g?.failed ? ' error' : '');
  banner.textContent = state.jobError || (restoring ? 'Возвращаем письмо в черновики…' : uploading ? 'Сохраняем вложения…' : state.sending ? 'Отправляем выбранные письма. Не закрывайте приложение до завершения.' : state.generating ? 'Готовим индивидуальные письма по описаниям компаний. Шаблон используется только без описания.' : summary);
  renderControls();
}
function attachmentHTML(lead, locked) {
  return `<div class="attachment-section"><div class="attachment-heading"><h2>Вложения ${lead.attachments?.length ? `· ${lead.attachments.length}` : ''}</h2>${!locked ? '<button type="button" id="editor-attach-button" class="button secondary attachment-mutation">＋ Прикрепить</button><input type="file" id="editor-attachment-input" multiple hidden>' : ''}</div>
    <div class="attachment-list">${(lead.attachments || []).map(f => `<div class="attachment-item"><span class="attachment-icon">▧</span><a class="attachment-name" href="/api/attachment?leadId=${encodeURIComponent(lead.id)}&id=${encodeURIComponent(f.id)}&name=${encodeURIComponent(f.name)}" download>${escapeHTML(f.name)}<small>${sizeText(f.size)}</small></a>${!locked ? `<button type="button" class="attachment-remove attachment-mutation" data-remove-attachment="${escapeHTML(f.id)}" data-filename="${escapeHTML(f.name)}" aria-label="Удалить ${escapeHTML(f.name)}">×</button>` : ''}</div>`).join('') || '<p class="field-hint">Файлы не прикреплены.</p>'}</div>
    ${!locked ? '<p class="field-hint">До 10 МБ на файл, 15 МБ на письмо. Вложения сохраняются сразу.</p>' : ''}</div>`;
}
function deliveryHistoryHTML(lead) {
  if (!lead.sendHistory?.length) return '';
  return `<details class="delivery-history"><summary>История отправок · ${lead.sendHistory.length}</summary><ul>${lead.sendHistory.map(record => `<li><strong>${escapeHTML(record.sentAt ? new Date(record.sentAt).toLocaleString('ru-RU') : 'Дата не указана')}</strong><span>${escapeHTML((record.recipients || []).join(', '))}</span>${record.messageId ? `<code>${escapeHTML(record.messageId)}</code>` : ''}</li>`).join('')}</ul></details>`;
}
function renderEditor() {
  const lead = leadByID(activeID); if (!lead) return;
  const locked = ['sent','sending'].includes(lead.status) || busy(), manual = lead.kind === 'message';
  const time = lead.sentAt ? new Date(lead.sentAt).toLocaleString('ru-RU') : '';
  $('#preview-panel').innerHTML = `
    <div class="preview-head"><div><div class="preview-label">${manual ? 'ОБЫЧНОЕ ПИСЬМО' : 'ПИСЬМО КОМПАНИИ'}</div><h2>${escapeHTML(lead.company)}</h2></div><span class="status-chip ${escapeHTML(lead.status)}">${statusText(lead)}</span></div>
    <form id="editor-form" class="editor">
      ${manual ? '' : `<label>Компания<input name="company" value="${escapeHTML(lead.company)}" required ${locked ? 'disabled' : ''}></label>`}
      <label>Кому${manual ? `<textarea name="recipients" rows="2" ${locked ? 'disabled' : ''}>${escapeHTML(recipientText(lead))}</textarea>` : `<input name="email" value="${escapeHTML(lead.email)}" placeholder="name@company.ru" ${locked ? 'disabled' : ''}>`}</label>
      ${manual ? '<p class="field-hint">Адреса через запятую или точку с запятой. Все получатели видят список «Кому».</p>' : ''}
      ${!validDestination(lead) ? '<div class="inline-note">Исправьте адрес, чтобы выбрать письмо для отправки.</div>' : ''}
      ${lead.error ? `<div class="inline-error">${escapeHTML(lead.error)}</div>` : ''}${lead.generationError ? `<div class="inline-error">Генерация не выполнена. Предыдущий текст не изменён: ${escapeHTML(lead.generationError)}</div>` : ''}
      ${lead.resend ? '<div class="inline-note">Возвращено для повторной отправки. Можно изменить или подготовить письмо заново, затем выбрать его вместе с другими. Отправка потребует двух подтверждений.</div>' : ''}
      ${!manual ? `<div class="field-hint">${lead.source==='deepseek' ? 'Текст получен от DeepSeek на основе описания компании.' : lead.source==='manual' ? 'Текст отредактирован вручную.' : lead.source==='template' ? 'Использован шаблон: при подготовке описание отсутствовало. Добавьте описание ниже и запустите генерацию заново.' : lead.description?.trim() ? 'При подготовке DeepSeek получит описание этой компании и ваше задание.' : 'Описание не заполнено — будет использован шаблон по умолчанию.'}</div>` : ''}
      <label>Тема<input name="subject" value="${escapeHTML(lead.subject)}" placeholder="Тема письма" maxlength="1000" ${locked ? 'disabled' : ''}></label>
      <label>Письмо<textarea name="body" rows="7" maxlength="50000" placeholder="Напишите сообщение или подготовьте черновик" ${locked ? 'disabled' : ''}>${escapeHTML(lead.body)}</textarea></label>
      <div class="editor-footer"><span id="editor-save-status">${lead.status === 'sent' ? `Принято SMTP · ${escapeHTML(time)}` : 'Изменения сохраняются кнопкой справа'}</span><button class="button secondary" type="submit" ${locked ? 'disabled' : ''}>Сохранить письмо</button></div>
      ${attachmentHTML(lead, locked)}
      ${deliveryHistoryHTML(lead)}
      ${lead.status === 'unknown' ? '<div class="inline-note">Проверьте доставку у провайдера. Только если письмо точно не принято, разрешите повтор.</div><button type="button" id="resolve-button" class="button danger">Проверил: письмо не принято</button>' : ''}
      ${manual ? '' : `<details class="description"><summary>Описание компании ${lead.description ? '' : '· отсутствует'}</summary><label>Контекст для DeepSeek<textarea name="description" rows="4" maxlength="50000" ${locked ? 'disabled' : ''} placeholder="Чем занимается компания, какие продукты выпускает…">${escapeHTML(lead.description)}</textarea></label></details>`}
      ${lead.messageId ? `<div class="field-hint message-id">Message-ID: ${escapeHTML(lead.messageId)}</div>` : ''}
    </form>`;
  editorDirty = false;
  $('#editor-form').addEventListener('input', e => {
    if (e.target.type === 'file') return;
    editorDirty = true; $('#editor-save-status').textContent = 'Есть несохранённые изменения'; $('#editor-save-status').className = 'dirty-note'; renderControls();
  });
  $('#editor-form').addEventListener('submit', e => {e.preventDefault();action(async () => {
    const data = Object.fromEntries(new FormData(e.target));
    if (manual) data.recipients = splitRecipients(data.recipients);
    await request('lead',{id:lead.id,...data}); editorDirty=false; await refresh(true); toast('Письмо сохранено');
  });});
  $('#editor-attach-button')?.addEventListener('click', () => $('#editor-attachment-input').click());
  $('#editor-attachment-input')?.addEventListener('change', e => action(async () => {try {await uploadAttachments([lead.id],[...e.target.files]);} finally {e.target.value='';}}));
  $$('[data-remove-attachment]').forEach(button => button.addEventListener('click', () => action(async () => {
    if (editorDirty) throw new Error('Сначала сохраните текст письма');
    await request('attachments/remove',{id:lead.id,attachmentId:button.dataset.removeAttachment,name:button.dataset.filename}); await refresh(true); toast('Вложение удалено');
  })));
  $('#resolve-button')?.addEventListener('click', () => action(async () => {
    if (!window.confirm('Вы проверили у провайдера, что письмо не было принято? Только после этого можно повторить отправку.')) return;
    await request('resolve',{id:lead.id,checked:true}); await refresh(true); toast('Повторная отправка разрешена');
  }));
  renderControls();
}
async function uploadAttachments(ids, files) {
  if (!files.length) return;
  if (editorDirty) throw new Error('Сначала сохраните текст письма');
  checkFiles(files);
  const form=new FormData(); form.append('ids',JSON.stringify(ids)); files.forEach(f => form.append('files',f));
  uploading=true; renderControls();
  try {const result=await request('attachments',form);toast(`Вложения сохранены. Обновлено писем: ${result.updated}.`);} finally {uploading=false;await refresh(true);}
}
function settingsData() {
  const data = Object.fromEntries(new FormData($('#settings-form')));
  data.port = Number(data.port);
  return data;
}
function settingsDirty() {
  return page === 'settings' && state && Object.entries(settingsData()).some(([key,value]) => value !== (state.settings[key] ?? ''));
}
function unsavedChanges() { return editorDirty || formDirty || settingsDirty(); }
function renderConnectionStatus() {
  const password = $('#settings-form').elements.password;
  password.placeholder = state.smtpPasswordSaved ? 'Оставьте пустым, чтобы сохранить текущий' : 'Введите пароль приложения Mail.ru';
  $('#smtp-password-status').textContent = password.value ? 'Введён новый пароль. Нажмите «Сохранить подключения», чтобы использовать его при отправке.' : state.smtpPasswordSaved ? 'Пароль сохранён на этом компьютере. Поле оставлено пустым; повторно вводить пароль не нужно.' : 'Пароль ещё не сохранён. Введите его и нажмите «Сохранить подключения».';
  $('#smtp-status').textContent = settingsDirty() ? 'Не сохранено' : state.smtpReady ? 'Данные сохранены' : 'Нужны данные';
  $('#deepseek-status').textContent = state.deepseekReady ? 'Ключ сохранён' : 'Нужен ключ';
}
function fillForms() {
  for (const key of ['subject','body','instructions','senderProfile']) $('#template-form').elements[key].value = state.project.template[key] || '';
  for (const [key,value] of Object.entries(state.settings)) if ($('#settings-form').elements[key]) $('#settings-form').elements[key].value = value;
  $('#settings-form').elements.password.value = ''; $('#settings-form').elements.apiKey.value = '';
  renderConnectionStatus();
}
async function refresh(editor = false) {
  const wasBusy = busy(), before = leadByID(activeID);
  state = await request('state');
  renderConnectionStatus();
  selected = new Set([...selected].filter(id => {const l=leadByID(id); return l && (selectable(l) || l.status === 'sending');}));
  renderList();
  const after = leadByID(activeID);
  if (editor || !editorDirty && (wasBusy || busy() || JSON.stringify(before) !== JSON.stringify(after))) renderEditor();
  clearTimeout(pollTimer);
  pollTimer = setTimeout(() => refresh().catch(error => {toast(error.message,true);pollTimer=setTimeout(()=>action(()=>refresh()),5000);}),busy() ? 1500 : 5000);
}
async function navigate(next) {
  if (next === page) return;
  if (unsavedChanges() && !window.confirm('Изменения не сохранены. Перейти без сохранения?')) return;
  if (page==='compose' && formDirty) {$('#compose-form').reset();composeFiles=[];renderComposeFiles();}
  editorDirty=false;formDirty=false;page=next;
  $$('.page').forEach(el => el.hidden = el.id !== `page-${page}`);
  $$('.nav-button').forEach(el => el.classList.toggle('active',el.dataset.page === page));
  $('#breadcrumb').textContent = {campaign:'Письма',compose:'Новое письмо',template:'Шаблон письма',settings:'Подключения'}[page];
  fillForms(); if (page === 'campaign') {renderList();renderEditor();}
}
function renderComposeFiles() {
  $('#compose-attachments').innerHTML=composeFiles.map((f,index)=>`<div class="attachment-item"><span class="attachment-icon">▧</span><span class="attachment-name">${escapeHTML(f.name)}<small>${sizeText(f.size)}</small></span><button type="button" class="attachment-remove" data-compose-remove="${index}" aria-label="Удалить ${escapeHTML(f.name)}">×</button></div>`).join('') || '<p class="field-hint">Файлы не прикреплены.</p>';
  $$('[data-compose-remove]').forEach(button=>button.addEventListener('click',()=>{composeFiles.splice(Number(button.dataset.composeRemove),1);formDirty=true;renderComposeFiles();}));
}
async function beginSend(ids) {
  if (editorDirty) throw new Error('Сначала сохраните изменения письма');
  sendingRequest=true;renderControls();
  try {
    const result=await request('prepare',{ids}); currentToken=result.token;confirmStage=1;
    $('#confirm-step').textContent='ПОДТВЕРЖДЕНИЕ 1 ИЗ 2';$('#confirm-title').textContent='Подтвердите отправку';
    $('#confirm-text').textContent=`Вы выбрали ${result.count} писем. Отправитель: ${result.from}.`;
    $('#confirm-recipients').textContent=ids.map(leadByID).filter(Boolean).map(l=>`${l.company} → ${recipientText(l)}${l.attachments?.length ? '\nВложения: '+l.attachments.map(f=>f.name).join(', ') : ''}`).join('\n\n');
    $('#confirm-note').textContent='Проверьте адресатов, текст и вложения. Далее будет последнее подтверждение.';
    $('#confirm-next').textContent='Подтвердить';$('#confirm-dialog').showModal();
  } finally {sendingRequest=false;renderControls();}
}

$$('.nav-button').forEach(el=>el.addEventListener('click',()=>navigate(el.dataset.page)));
$('#new-message-button').addEventListener('click',()=>navigate('compose'));
$$('.tab').forEach(el=>el.addEventListener('click',()=>{filter=el.dataset.filter;$$('.tab').forEach(tab=>tab.classList.toggle('active',tab===el));renderList();}));
$('#search').addEventListener('input',renderList);
$('#select-all').addEventListener('change',e=>{for (const l of visibleLeads().filter(selectable)){if(e.target.checked)selected.add(l.id);else selected.delete(l.id);}renderList();});
$('#lead-list').addEventListener('click',e=>{
  const restore=e.target.closest('[data-restore]');
  if(restore){
    e.stopPropagation();
    if(restore.disabled || busy() || editorDirty)return;
    action(async()=>{
      restoring=true;renderList();
      try{
        await request('restore',{id:restore.dataset.restore});
        selected.delete(restore.dataset.restore);activeID=restore.dataset.restore;
        if(filter==='sent'){filter='pending';$$('.tab').forEach(tab=>tab.classList.toggle('active',tab.dataset.filter===filter));}
        await refresh(true);
        toast('Письмо возвращено в черновики. Выберите его для повторной отправки или подготовки.');
      }finally{restoring=false;renderList();renderEditor();}
    });
    return;
  }
  const checkbox=e.target.closest('[data-check]');
  if(checkbox){e.stopPropagation();if(checkbox.disabled)return;if(checkbox.checked)selected.add(checkbox.dataset.check);else selected.delete(checkbox.dataset.check);renderList();return;}
  const row=e.target.closest('[data-id]');if(!row)return;
  if(editorDirty&&!window.confirm('Изменения не сохранены. Открыть другое письмо?'))return;
  activeID=row.dataset.id;renderList();renderEditor();
});
$('#lead-list').addEventListener('keydown',e=>{if((e.key==='Enter'||e.key===' ')&&e.target.matches('.lead-row')){e.preventDefault();e.target.click();}});
$('#import-button').addEventListener('click',()=>{if(unsavedChanges()){toast('Сначала сохраните изменения',true);return;}$('#file-input').click();});
$('#file-input').addEventListener('change',e=>action(async()=>{
  const file=e.target.files[0];if(!file)return;
  try{const form=new FormData();form.append('file',file);$('#import-button').disabled=true;const result=await request('import',form);await refresh(true);fillForms();toast(`Добавлено: ${result.added}. Уже были в проекте: ${result.skipped}.`);}finally{e.target.value='';$('#import-button').disabled=busy();}
}));
$('#export-menu-button').addEventListener('click',()=>{const menu=$('#export-menu');menu.hidden=!menu.hidden;$('#export-menu-button').setAttribute('aria-expanded',String(!menu.hidden));});
document.addEventListener('click',e=>{if(!e.target.closest('.export-wrap')){$('#export-menu').hidden=true;$('#export-menu-button').setAttribute('aria-expanded','false');}});
$('#bulk-attach-button').addEventListener('click',()=>$('#bulk-attachment-input').click());
$('#bulk-attachment-input').addEventListener('change',e=>action(async()=>{try{await uploadAttachments([...selected],[...e.target.files]);}finally{e.target.value='';}}));
$('#generate-button').addEventListener('click',()=>action(async()=>{
  const companies=selectedLeads().filter(l=>l.kind!=='message');
  if(companies.some(l=>l.body)&&!window.confirm('Заменить черновики выбранных компаний? Ручные изменения будут заменены.'))return;
  await request('generate',{ids:companies.map(l=>l.id)});await refresh(true);
}));
$('#compose-form').addEventListener('input',()=>formDirty=true);
$('#compose-attach-button').addEventListener('click',()=>$('#compose-attachment-input').click());
$('#compose-attachment-input').addEventListener('change',e=>action(async()=>{
  try{const files=[...composeFiles];for(const f of e.target.files){if(!files.some(p=>p.name===f.name&&p.size===f.size&&p.lastModified===f.lastModified))files.push(f);}checkFiles(files);composeFiles=files;formDirty=true;renderComposeFiles();}finally{e.target.value='';}
}));
$('#compose-form').addEventListener('submit',e=>{
  e.preventDefault();const mode=e.submitter?.dataset.mode;
  action(async()=>{
    if(busy())throw new Error('Дождитесь завершения операции');
    const fields=Object.fromEntries(new FormData(e.target));const to=splitRecipients(fields.recipients);
    if(!to.length||!to.every(validEmail))throw new Error('Проверьте адреса получателей');
    if(mode==='send'&&(!fields.subject.trim()||!fields.body.trim()))throw new Error('Перед отправкой укажите тему и текст');
    const data=new FormData();data.append('message',JSON.stringify({recipients:to,subject:fields.subject,body:fields.body}));composeFiles.forEach(f=>data.append('files',f));
    uploading=true;renderControls();
    let result;try{result=await request('message',data);}finally{uploading=false;renderControls();}
    e.target.reset();composeFiles=[];renderComposeFiles();formDirty=false;
    await refresh();activeID=result.id;selected=new Set([result.id]);filter='all';$('#search').value='';$$('.tab').forEach(tab=>tab.classList.toggle('active',tab.dataset.filter==='all'));
    await navigate('campaign');renderList();renderEditor();toast('Черновик с вложениями сохранён');
    if(mode==='send')await beginSend([result.id]);
  });
});
$('#template-form').addEventListener('input',()=>formDirty=true);
for (const event of ['input','change']) $('#settings-form').addEventListener(event,()=>{formDirty=true;$('#connection-result').textContent='';renderConnectionStatus();});
$('#template-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{await request('template',Object.fromEntries(new FormData(e.target)));formDirty=false;await refresh();fillForms();toast('Шаблон сохранён');});});
$('#settings-form').addEventListener('submit',e=>{e.preventDefault();action(async()=>{await request('settings',settingsData());formDirty=false;await refresh();fillForms();$('#connection-result').textContent='';toast('Подключения сохранены');});});
$('#smtp-test').addEventListener('click',()=>action(async()=>{
  if(!$('#settings-form').reportValidity())return;
  renderConnectionStatus();
  const button=$('#smtp-test');button.disabled=true;$('#connection-result').textContent='Проверяем соединение и авторизацию…';
  try{const result=await request('smtp-test',{settings:settingsData()});$('#connection-result').textContent=result.message+(settingsDirty()?' Настройки ещё не сохранены. Нажмите «Сохранить подключения» перед отправкой.':'');toast('SMTP проверен, письма не отправлялись');}catch(e){$('#connection-result').textContent=e.message;throw e;}finally{button.disabled=false;}
}));
$('#send-button').addEventListener('click',()=>action(()=>beginSend([...selected])));
$('#confirm-next').addEventListener('click',()=>action(async()=>{
  const button=$('#confirm-next');button.disabled=true;
  try{
    if(confirmStage===1){const result=await request('confirm',{token:currentToken});currentToken=result.token;confirmStage=2;$('#confirm-step').textContent='ПОДТВЕРЖДЕНИЕ 2 ИЗ 2';$('#confirm-title').textContent='Точно подтверждаете?';$('#confirm-note').textContent='После нажатия письма с вложениями начнут уходить получателям. Отменить уже принятое сервером письмо нельзя.';button.textContent='Да, точно подтверждаю';}
    else if(confirmStage===2){await request('send',{token:currentToken});currentToken='';confirmStage=0;$('#confirm-dialog').close();await refresh(true);toast('Отправка запущена');}
  }catch(e){$('#confirm-dialog').close();throw e;}finally{button.disabled=false;}
}));
$('#confirm-dialog').addEventListener('close',()=>{currentToken='';confirmStage=0;});
$('#stop-button').addEventListener('click',()=>action(async()=>{await request('stop',{});toast('Очередь остановится после текущего запроса');}));
$('#quit-button').addEventListener('click',()=>action(async()=>{if(!window.confirm('Завершить работу MailDesk? Сохранённые данные останутся на компьютере.'))return;await request('quit',{});clearTimeout(pollTimer);toast('MailDesk завершён. Для продолжения запустите приложение снова.');$$('button').forEach(b=>b.disabled=true);}));
window.addEventListener('beforeunload',e=>{if(unsavedChanges()){e.preventDefault();e.returnValue='';}});
action(async()=>{await refresh();fillForms();renderComposeFiles();});
