import { postJson } from "./api";

function decode(value:string):ArrayBuffer {
 const raw=atob(value.replace(/-/g,"+").replace(/_/g,"/"));
 return Uint8Array.from(raw,c=>c.charCodeAt(0)).buffer;
}
function encode(value:ArrayBuffer):string {
 return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g,"-").replace(/\//g,"_").replace(/=+$/,"");
}
type Descriptor=Omit<PublicKeyCredentialDescriptor,"id">&{id:string};
type RequestJSON=Omit<PublicKeyCredentialRequestOptions,"challenge"|"allowCredentials">&{challenge:string;allowCredentials?:Descriptor[]};
type CreationJSON=Omit<PublicKeyCredentialCreationOptions,"challenge"|"user"|"excludeCredentials">&{challenge:string;user:Omit<PublicKeyCredentialUserEntity,"id">&{id:string};excludeCredentials?:Descriptor[]};
function serialize(credential:PublicKeyCredential){
 const response=credential.response;
 const common={id:credential.id,rawId:encode(credential.rawId),type:credential.type,authenticatorAttachment:credential.authenticatorAttachment,clientExtensionResults:credential.getClientExtensionResults()};
 if(response instanceof AuthenticatorAttestationResponse)return {...common,response:{clientDataJSON:encode(response.clientDataJSON),attestationObject:encode(response.attestationObject),transports:response.getTransports()}};
 const assertion=response as AuthenticatorAssertionResponse;
 return {...common,response:{clientDataJSON:encode(assertion.clientDataJSON),authenticatorData:encode(assertion.authenticatorData),signature:encode(assertion.signature),userHandle:assertion.userHandle?encode(assertion.userHandle):null}};
}
export async function assertPasskey(optionsPath:string,verifyPath:string,extra:Record<string,unknown>={}){
 const options=await postJson<{}, {challenge_id:string;publicKey:RequestJSON}>(optionsPath,{}, {redirectOnUnauthorized:false});
 const {allowCredentials,...request}=options.publicKey;
 const publicKey:PublicKeyCredentialRequestOptions={...request,challenge:decode(options.publicKey.challenge),...(options.publicKey.allowCredentials?{allowCredentials:options.publicKey.allowCredentials.map(c=>({...c,id:decode(c.id)}))}:{})};
 const credential=await navigator.credentials.get({publicKey}) as PublicKeyCredential|null;
 if(!credential)throw new Error("Passkey sign-in was cancelled.");
 return postJson<Record<string,unknown>,{redirect_after?:string}|undefined>(verifyPath,{...extra,challenge_id:options.challenge_id,credential:serialize(credential)},{redirectOnUnauthorized:false});
}
export async function registerPasskey(optionsPath:string,completePath:string,label:string){
 const options=await postJson<{}, {challenge_id:string;publicKey:CreationJSON}>(optionsPath,{}, {redirectOnUnauthorized:false});
 const {excludeCredentials,...creation}=options.publicKey;
 const publicKey:PublicKeyCredentialCreationOptions={...creation,challenge:decode(options.publicKey.challenge),user:{...options.publicKey.user,id:decode(options.publicKey.user.id)},...(options.publicKey.excludeCredentials?{excludeCredentials:options.publicKey.excludeCredentials.map(c=>({...c,id:decode(c.id)}))}:{})};
 const credential=await navigator.credentials.create({publicKey}) as PublicKeyCredential|null;
 if(!credential)throw new Error("Passkey registration was cancelled.");
 return postJson<Record<string,unknown>,{enrollment_id:string;next:string}|undefined>(completePath,{challenge_id:options.challenge_id,credential:serialize(credential),label},{redirectOnUnauthorized:false});
}
