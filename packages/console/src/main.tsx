import { trackAccountActivity } from "./lib/activity";
import { render } from "solid-js/web";
import { QueryClient, QueryClientProvider } from "@tanstack/solid-query";
import { App } from "./app";
import "./styles.css";

const root = document.getElementById("root");
if (!root) {
  throw new Error("root element is missing");
}

const queryClient = new QueryClient();
trackAccountActivity();
window.addEventListener("storage",event=>{
 if(event.key!=="helmr.account-change")return;
 void fetch("/api/me",{credentials:"same-origin",cache:"no-store"}).then(async response=>{
  const account=response.ok?await response.json() as {user_id:string}:undefined;
  if(account && account.user_id!==sessionStorage.getItem("helmr.account")) {
   sessionStorage.removeItem("helmr.organization");window.location.reload();
  } else if(!account) {
   window.dispatchEvent(new CustomEvent("helmr-confirmation-required",{detail:{operator:false,signIn:true}}));
  }
 }).catch(()=>{});
});

render(
  () => (
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  ),
  root,
);
