// A trusted foreground interaction, not query polling or a timer, refreshes
// ordinary inactivity. Responses deliberately carry no session cookie.
export function trackAccountActivity(){
 let last=0;
 const record=(event:Event)=>{
  if(!event.isTrusted||document.visibilityState!=="visible"||Date.now()-last<60_000)return;
  if(!document.querySelector('[data-account-active]'))return;
  last=Date.now();
  void fetch("/api/account/activity",{method:"POST",credentials:"same-origin",headers:{"content-type":"application/json"},body:"{}"}).catch(()=>{});
 };
 window.addEventListener("pointerdown",record,{passive:true});window.addEventListener("keydown",record,{passive:true});
 return ()=>{window.removeEventListener("pointerdown",record);window.removeEventListener("keydown",record);};
}
