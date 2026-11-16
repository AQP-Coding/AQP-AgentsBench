const select=document.querySelector('#runs');
async function refresh(){try{
 const runs=await fetch('/api/runs').then(r=>{if(!r.ok)throw Error(r.status);return r.json()});
 const previous=select.value;select.replaceChildren();
 for(const run of runs){const option=document.createElement('option');option.value=run.id;
 option.textContent=run.id+' · '+run.status;select.append(option)}
 if(runs.some(r=>r.id===previous))select.value=previous;
 if(select.value){const base='/api/runs/'+encodeURIComponent(select.value);
 const detail=await fetch(base).then(r=>r.json());
 document.querySelector('#details').textContent=JSON.stringify(detail,null,2);
 const events=await fetch(base+'/events').then(r=>r.json());
 document.querySelector('#events').textContent=events.map(e=>JSON.stringify(e)).join('\n');}
 document.querySelector('#status').textContent=runs.length+' runs · updated '+new Date().toLocaleTimeString();
}catch(e){document.querySelector('#status').textContent='Connection unavailable: '+e.message}}
select.addEventListener('change',refresh);refresh();setInterval(refresh,1000);