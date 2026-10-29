// Browser storage for HTTP-client unit tests running outside a DOM.
import { beforeEach } from "bun:test";
beforeEach(() => {
 for (const name of ["sessionStorage","localStorage"]) {
  const values = new Map<string,string>();
  Object.defineProperty(globalThis,name,{configurable:true,value:{getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>values.set(key,value),removeItem:(key:string)=>values.delete(key),clear:()=>values.clear()}});
 }
});
