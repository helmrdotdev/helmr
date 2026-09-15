package guestd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Uses the normal OpenCode SDK and real control server, without a model request.
// Run in the disposable Linux fixture with pinned externally supplied binaries.
func TestProtectedEnvNativeOpenCodeLoopback(t *testing.T) {
	binary, module := os.Getenv("HELMR_TEST_OPENCODE_BINARY"), os.Getenv("HELMR_TEST_OPENCODE_SDK")
	if binary == "" || module == "" {
		t.Skip("requires pinned local OpenCode server and SDK fixture")
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic Workspace"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	root := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH")}
	if err := stageProtectedEnv(root, map[string]string{"FACTORY_MODEL_TOKEN": "hlmr_protected_" + strings.Repeat("a", 64)}, ca, &env); err != nil {
		t.Fatal(err)
	}
	// The test has not mounted imageRoot as /. Translate only staged public file
	// locations; keep the actual staged routing/selector environment unchanged.
	for i, v := range env {
		key, value, _ := strings.Cut(v, "=")
		if key == "SSL_CERT_FILE" || key == "NODE_EXTRA_CA_CERTS" {
			env[i] = key + "=" + filepath.Join(root, value)
		}
	}
	env = append(env, "FIXTURE_OPENCODE="+binary, "FIXTURE_SDK="+module, "FIXTURE_STATE="+root)
	script := `
import {spawn} from 'node:child_process';
import {mkdir} from 'node:fs/promises';
import {pathToFileURL} from 'node:url';
const {createOpencodeClient}=await import(pathToFileURL(process.env.FIXTURE_SDK));
for(const key of ['HTTP_PROXY','HTTPS_PROXY','http_proxy','https_proxy','ALL_PROXY','NO_PROXY','NODE_USE_ENV_PROXY'])if(process.env[key]!==undefined)throw new Error('injected routing '+key);
const state=process.env.FIXTURE_STATE;
for(const d of ['home','data','config','cache','state','work'])await mkdir(state+'/'+d,{recursive:true});
const child=spawn(process.env.FIXTURE_OPENCODE,['serve','--hostname=127.0.0.1','--port=0'],{cwd:state+'/work',env:{...process.env,HOME:state+'/home',XDG_DATA_HOME:state+'/data',XDG_CONFIG_HOME:state+'/config',XDG_CACHE_HOME:state+'/cache',XDG_STATE_HOME:state+'/state',OPENCODE_CONFIG_CONTENT:JSON.stringify({share:'disabled',autoupdate:false,plugin:[],lsp:false,enabled_providers:[]}),OPENCODE_DISABLE_PROJECT_CONFIG:'true',OPENCODE_DISABLE_MODELS_FETCH:'true'},stdio:['ignore','pipe','pipe']});
const exited=new Promise(r=>child.once('exit',r));
let stderr='';child.stderr.on('data',d=>stderr+=d);
try{
 const url=await new Promise((resolve,reject)=>{let output='';const timer=setTimeout(()=>reject(new Error('startup timeout '+stderr)),20000);child.once('error',reject);child.once('exit',()=>reject(new Error('server exited '+stderr)));child.stdout.on('data',d=>{output+=d;const m=output.match(/opencode server listening on (http:\/\/127\.0\.0\.1:\d+)/);if(m){clearTimeout(timer);resolve(m[1])}})});
 const client=createOpencodeClient({baseUrl:url,directory:state+'/work',throwOnError:true});
 const health=await client.global.health({signal:AbortSignal.timeout(5000)});
 if(health.data?.version!=='1.18.30')throw new Error('wrong server version');
 const session=await client.session.create({title:'protected egress local control fixture'},{signal:AbortSignal.timeout(5000)});
 if(!session.data?.id)throw new Error('session creation failed');
 console.log(JSON.stringify({server:health.data.version,normalSDK:true,health:true,sessionCreated:true,modelCalls:0,proxyEnv:false}));
}finally{child.kill('SIGTERM');const timer=setTimeout(()=>child.kill('SIGKILL'),2000);await exited;clearTimeout(timer);}
`
	cmd := exec.Command("node", "--input-type=module", "-e", script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ordinary SDK/control server: %v: %s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
