(() => {
  if (!document.body) return null;
  const cache = window.__bnblocBrowse ||= {ids:new WeakMap(), nodes:new Map(), next:1};
  // A navigation to another document the page began (a script's redirect, a form) shows nothing
  // until it arrives: the page reports when it began.
  if (!cache.watching && window.navigation) {
    cache.watching=true;
    navigation.addEventListener('navigate',e=>{ if (!e.destination.sameDocument && e.downloadRequest===null) cache.left=performance.now(); });
  }
  const identity = e => {
    if (!cache.ids.has(e)) cache.ids.set(e,cache.next++);
    const id=cache.ids.get(e); cache.nodes.set(id,e); return id;
  };
  for (const [id,e] of cache.nodes) if (!e.isConnected) cache.nodes.delete(id);
  // A secret field is observed by its label only: its value is never read.
  const secretAutocomplete=['current-password','new-password','one-time-code','cc-number','cc-csc',
    'cc-exp','cc-exp-month','cc-exp-year'];
  const secret = e => e.type==='password' ||
    secretAutocomplete.some(t=>(e.getAttribute('autocomplete')||'').split(/\s+/).includes(t));
  const safe = e => e.type!=='hidden' && e.type!=='file' && !secret(e);
  const visible = e => !e.closest('[aria-hidden="true"],[inert]') &&
    e.checkVisibility({checkOpacity:true,checkVisibilityCSS:true});
  const name = (e,seen=new Set()) => {
    if (!e || seen.has(e)) return '';
    seen.add(e);
    const referenced=(e.getAttribute('aria-labelledby')||'').split(/\s+/)
      .map(id=>name(document.getElementById(id),seen)).filter(Boolean).join(' ');
    return referenced || e.getAttribute('aria-label') ||
      [...(e.labels||[])].map(l=>name(l,seen)).filter(Boolean).join(' ') ||
      (['button','submit','reset'].includes(e.type) ? e.value : '') || e.getAttribute('alt') ||
      (e.tagName==='INPUT' ? '' : [...e.childNodes].map(n=>n.nodeType===3 ? n.textContent :
        n.nodeType===1 && n.getAttribute('aria-hidden')!=='true' ? name(n,seen) : '').join(' ').trim()) ||
      e.getAttribute('title') || e.getAttribute('placeholder') || '';
  };
  const roles=['button','link','checkbox','radio','switch','tab','menuitem','menuitemradio',
    'option','gridcell','combobox','textbox','searchbox','spinbutton'];
  const selector='a[href],button,input,textarea,select,summary,[contenteditable="true"],'+
    roles.map(role=>'[role="'+role+'"]').join(',');
  const role = e => {
    const explicit=e.getAttribute('role');
    if (roles.includes(explicit)) return explicit;
    if (e.tagName==='BUTTON' || e.tagName==='SUMMARY') return 'button';
    if (e.tagName==='A') return 'link';
    if (e.tagName==='SELECT') return 'combobox';
    if (e.tagName==='TEXTAREA' || e.isContentEditable) return 'textbox';
    if (e.tagName==='INPUT') {
      if (['checkbox','radio'].includes(e.type)) return e.type;
      if (['button','submit','reset','image'].includes(e.type)) return 'button';
      if (e.type==='search') return 'searchbox';
      if (e.type==='number') return 'spinbutton';
      if (e.type==='file') return 'file';
      if (['text','email','url','tel','password'].includes(e.type)) return 'textbox';
    }
    return null;
  };
  cache.pageKey=()=>[performance.timeOrigin,location.href,scrollX,scrollY,innerWidth,innerHeight,
    [...document.querySelectorAll('input,textarea,select')].filter(safe)
      .map(e=>[identity(e),e.value,e.checked,e.selectedIndex,e.disabled,e.readOnly])];
  cache.guard=e=>{
    if (!e?.isConnected || !visible(e)) return null;
    const scope=e.closest('form,dialog,[role="dialog"],article,li,tr,[role="row"]') || e.parentElement;
    return [identity(e),role(e),name(e),safe(e) ? e.value??null : null,e.checked??null,e.selectedIndex??null,
      e.readOnly??null,e.matches(':disabled'),e.getAttribute('aria-disabled'),
      e.getAttribute('aria-expanded'),e.getAttribute('aria-checked'),e.getAttribute('aria-selected'),
      e.getAttribute('href'),scope?.innerText?.slice(0,6000)||''];
  };
  const actions=[], listed=new Set();
  for (const e of document.querySelectorAll(selector)) {
    if (e.type==='hidden' || !visible(e) || e.matches(':disabled') || e.closest('[aria-disabled="true"]')) continue;
    const r=e.getBoundingClientRect(), x=r.x+r.width/2, y=r.y+r.height/2, rname=role(e);
    // The whole page's controls, those off-screen marked: the agent reads
    // the page at once, and an action scrolls its element into view.
    if (!rname || r.width<=0 || r.height<=0) continue;
    if (rname==='gridcell' && e.querySelector('button,[role="button"]')) continue;
    listed.add(e);
    const base={node:identity(e),role:rname,label:name(e)||rname,
      rect:{x:r.x,y:r.y,w:r.width,h:r.height}};
    if (x<0 || y<0 || x>=innerWidth || y>=innerHeight) base.offscreen=true;
    for (const key of ['checked','selected','expanded']) {
      const value=e.getAttribute('aria-'+key);
      if (value!==null) base[key]=value;
    }
    if (['checkbox','radio'].includes(e.type)) base.checked=String(e.checked);
    if (e.tagName==='A' && e.href) base.href=e.href;
    if (e.type==='file') {
      actions.push({...base,kind:'upload',value:'',multiple:e.multiple,accept:e.accept||''});
    } else if (secret(e)) {
      actions.push({...base,kind:'secret',value:'',filled:e.value!==''});
    } else if (e.tagName==='SELECT') {
      for (const o of e.options) if (!o.selected && !o.disabled && !o.closest('optgroup[disabled]'))
        actions.push({...base,kind:'select',value:o.value,
          current_value:[...e.selectedOptions].map(o=>o.label).join(', '),label:base.label+' → '+o.label});
    } else {
      const editable=!e.readOnly && e.getAttribute('aria-readonly')!=='true' &&
        (['textbox','searchbox','spinbutton'].includes(rname) ||
          (rname==='combobox' && ['INPUT','TEXTAREA'].includes(e.tagName)));
      const value='value' in e ? String(e.value) :
        e.isContentEditable || rname==='combobox' ? e.innerText.trim() : '';
      actions.push({...base,kind:editable?'fill':'click',value});
      if (editable) actions.push({...base,kind:'click',value,label:'Open '+base.label});
    }
  }
  // An element a script made clickable (a component library's menu item, tab
  // or card: a div or li whose click the framework binds) says so only by its
  // pointer cursor, or by an onclick attribute. The outermost element showing
  // the pointer is the control (its text spans inherit the cursor). One inside
  // a listed control, or holding one, is left to that control; a label is its
  // field's, unless the field is hidden (a styled checkbox).
  const holds=new Set();
  for (const e of listed) for (let p=e.parentElement; p && !holds.has(p); p=p.parentElement) holds.add(p);
  const cursors=new Map();
  const pointer = e => {
    if (!e || e===document.body) return false;
    if (!cursors.has(e)) cursors.set(e,getComputedStyle(e).cursor==='pointer');
    return cursors.get(e);
  };
  for (const e of document.body.querySelectorAll('*')) {
    if (!(e.hasAttribute('onclick') || pointer(e) && !pointer(e.parentElement))) continue;
    if (holds.has(e) || e.closest(selector)) continue;
    const field=e.closest('label')?.control;
    if (listed.has(field) || !visible(e) || e.closest('[aria-disabled="true"]')) continue;
    const label=name(e).replace(/\s+/g,' ').trim().slice(0,120), r=e.getBoundingClientRect();
    if (!label || r.width<=0 || r.height<=0) continue;
    const base={node:identity(e),role:'button',label,rect:{x:r.x,y:r.y,w:r.width,h:r.height}};
    const x=r.x+r.width/2, y=r.y+r.height/2;
    if (x<0 || y<0 || x>=innerWidth || y>=innerHeight) base.offscreen=true;
    if (['checkbox','radio'].includes(field?.type)) Object.assign(base,{role:field.type,checked:String(field.checked)});
    actions.push({...base,kind:'click',value:''});
  }
  const words=[], walker=document.createTreeWalker(document.body,NodeFilter.SHOW_TEXT);
  const range=document.createRange(); let node,length=0;
  while ((node=walker.nextNode()) && length<12000) {
    const value=node.textContent.trim(), parent=node.parentElement;
    if (!value || !parent || parent.closest('script,style,noscript,template') || !visible(parent)) continue;
    range.selectNodeContents(node); const r=range.getBoundingClientRect();
    if (r.width>0 && r.height>0) {
      words.push(value); length+=value.length;
    }
  }
  const text=words.join('\n').slice(0,12000), height=document.documentElement.scrollHeight;
  const page_key=cache.pageKey(), guards={};
  for (const a of actions) if (!(a.node in guards)) guards[a.node]=cache.guard(cache.nodes.get(a.node));
  // Compare meaning and identity. Geometry is always resolved and hit-tested just before input.
  const semantics=actions.map(({rect,...action})=>action);
  const marker=[performance.timeOrigin,location.href,scrollX,scrollY,innerWidth,innerHeight,
    document.title,text,semantics,page_key[6]];
  const omitted_actions=Math.max(0,actions.length-400);
  actions.splice(400);
  actions.forEach((a,i)=>a.id='e'+(i+1));
  if (scrollY+innerHeight<height-2) actions.push({id:'scroll_down',kind:'scroll',label:'Scroll down',delta:560});
  if (scrollY>0) actions.push({id:'scroll_up',kind:'scroll',label:'Scroll up',delta:-560});
  actions.push({id:'wait',kind:'wait',label:'Wait for the page to update'});
  const loaded=performance.getEntriesByType('navigation')[0]?.loadEventEnd;
  return {url:location.href,title:document.title,w:innerWidth,h:innerHeight,text,
    scroll:{y:scrollY,height},actions,marker,page_key,guards,omitted_actions,
    left_ms:cache.left ? performance.now()-cache.left : null,loaded_ms:loaded ? performance.now()-loaded : null};
})()
