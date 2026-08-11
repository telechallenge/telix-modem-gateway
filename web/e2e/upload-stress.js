// Stress harness for large ZMODEM uploads through the WHOLE stack:
//   browser bridge -> web proxy -> the real Go gateway -> lrzsz `rz`
//
// Every other live test in this repo talks to `rz` directly, so none of them
// exercise the gateway's data loop — where telnet IAC doubling and the `+++`
// escape detector live — or the proxy's backpressure. Both only misbehave at
// scale, which is exactly the shape of the failure this exists to chase: a
// 121,779,977-byte upload that succeeds direct-to-`rz` in 73s, succeeds through
// the full stack at 6 MB and 33 MB, and fails through the full stack more often
// than not at full size, with `rz` deleting the partial file and printing no
// protocol errors at all.
//
// Requires: lrzsz, and `make build` first (it runs ./telix). Ports 2444/3990.
//
//   SIZE=121779977 RUNS=3 node --max-old-space-size=4096 e2e/upload-stress.js
//
// A failing run can sit for the full 600s race, so budget accordingly.
const net=require('node:net'),fs=require('node:fs'),os=require('node:os'),path=require('node:path');
const crypto=require('node:crypto'),vm=require('node:vm');const {spawn}=require('node:child_process');
const {WebSocket}=require('ws');
const WEB=path.join(__dirname,'..');   // web/
const ROOT=path.join(WEB,'..');        // repo root
function loadSandbox(){const g={console:{log(){},warn(){},error(){},debug(){}},setTimeout,clearTimeout,setInterval,clearInterval,
  performance,Uint8Array,TextDecoder,Blob:class{constructor(c){this.chunks=c;}},WebSocket:{OPEN:1,CONNECTING:0}};g.window=g;vm.createContext(g);
  for(const f of ['cp437.js','xfer-util.js','vendor/zmodem.min.js','zmodem-sentry.js'])
    vm.runInContext(fs.readFileSync(path.join(WEB,'public','js',f),'utf8'),g,{filename:f});return g;}
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
// Ports are picked free per run: hardcoding them meant one leaked gateway from
// an aborted run blocked every later one with "address already in use".
function freePort(){return new Promise(res=>{const srv=net.createServer();
  srv.listen(0,'127.0.0.1',()=>{const p=srv.address().port;srv.close(()=>res(p));});});}

(async()=>{
  const SIZE=parseInt(process.env.SIZE||String(6*1024*1024),10);
  const RUNS=parseInt(process.env.RUNS||'5',10);
  const drop=fs.mkdtempSync(path.join(os.tmpdir(),'stack-'));
  const block=crypto.randomBytes(1<<20);
  const payload=Buffer.alloc(SIZE);
  for(let o=0;o<SIZE;o+=block.length) block.copy(payload,o,0,Math.min(block.length,SIZE-o));

  // Fake BBS: telnet-negotiates like ENiGMA, then runs rz on demand. Built per
  // run: a shared listener let one run's rz exit resolve the next run's promise.
  let rzDone=null, rzProc=null, rzErr='', closes=[];
  // Byte counters per hop. A stall names the hop that froze, which is the whole
  // point: bridge-sent is what zmodem.js handed the socket, bbs-in is what came
  // out the far end after telnet stripping, i.e. what rz actually sees.
  let sentBytes=0, bbsBytes=0, rzOutBytes=0;
  function makeBBS(){ return net.createServer(c=>{
    c.on('error',()=>{});
    c.write(Buffer.from([255,251,3, 255,251,1]));       // IAC WILL SGA / WILL ECHO
    rzProc=spawn('rz',['--zmodem','--binary','--overwrite'],{cwd:drop,stdio:['pipe','pipe','pipe']});
    rzProc.stderr.on('data',d=>{rzErr+=d;});
    rzProc.stdin.on('error',()=>{});
    const st={iac:false,sub:false,skip:false};
    c.on('data',d=>{ // strip telnet the way ENiGMA does before handing to rz
      const out=[];
      for(const b of d){
        if(st.sub){ if(st.iac&&b===240){st.sub=false;st.iac=false;} else st.iac=(b===255); continue; }
        if(st.skip){st.skip=false;continue;}
        if(st.iac){st.iac=false; if(b===255)out.push(255); else if(b===250)st.sub=true; else if(b>=251&&b<=254)st.skip=true; continue;}
        if(b===255){st.iac=true;continue;} out.push(b);
      }
      bbsBytes+=out.length;
      if(rzProc.stdin.writable) rzProc.stdin.write(Buffer.from(out));
    });
    rzProc.stdout.on('data',d=>{ rzOutBytes+=d.length; if(!c.destroyed) c.write(d); });
    rzProc.on('exit',code=>{ if(rzDone) rzDone(code); });
    c.on('close',()=>{closes.push('bbs-socket-closed');rzProc.kill();});
  }); }
  const bbs=makeBBS();
  await new Promise(r=>bbs.listen(0,'127.0.0.1',r));

  // The real gateway.
  const GW_PORT=await freePort(), PROXY_PORT=await freePort();
  const cfgPath=path.join(drop,'telix.yaml');
  fs.writeFileSync(cfgPath, `server: {port: ${GW_PORT}, max_connections: 20, max_per_ip: 20, idle_timeout: 900}\n`+
    `logging: {level: info, format: json}\nphonebook:\n`+
    `  - {number: "555-1212", host: "127.0.0.1", port: ${bbs.address().port}, name: "fungus"}\n`);
  const NO_GW=process.env.NO_GATEWAY==='1';
  let gw=null, gwlog='';
  if(!NO_GW){
    gw=spawn(path.join(ROOT,'telix'),['-config',cfgPath],{stdio:['ignore','pipe','pipe']});
    gw.stdout.on('data',d=>gwlog+=d); gw.stderr.on('data',d=>gwlog+=d);
    await new Promise((res,rej)=>{const t=setTimeout(()=>rej(new Error('gateway did not start: '+gwlog)),8000);
      const iv=setInterval(()=>{if(gwlog.includes('server_started')){clearInterval(iv);clearTimeout(t);res();}},100);});
  }

  // The web proxy, pointed at the gateway.
  const proxy=spawn('node',[path.join(WEB,'server.js')],
    {env:{...process.env,TELIX_HOST:'127.0.0.1',
      TELIX_PORT:String(NO_GW?bbs.address().port:GW_PORT),PORT:String(PROXY_PORT)},stdio:['ignore','pipe','inherit']});
  await new Promise((res,rej)=>{const t=setTimeout(()=>rej(new Error('no proxy')),8000);
    proxy.stdout.on('data',d=>{if(String(d).includes('listening')){clearTimeout(t);res();}});});

  let failures=0;
  for(let run=1; run<=RUNS; run++){
    const runStart=Date.now();
    sentBytes=0; bbsBytes=0; rzOutBytes=0;
    const g=loadSandbox();
    const ws=new WebSocket(`ws://127.0.0.1:${PROXY_PORT}/ws`);
    ws.binaryType='arraybuffer';
    await new Promise(r=>ws.on('open',r));
    const shim={readyState:1,get bufferedAmount(){return ws.bufferedAmount;},
      send(b){sentBytes+=b.length;ws.send(Buffer.from(b));}};
    const events=[],traces=[];
    g.window.ZmodemUI={startXfer(){},updateXfer(){},endXfer:d=>events.push(d.status),surfaceError(){},surfaceDownload(){},
      promptUpload:()=>Promise.resolve([{name:'Simcity 2000.zip',size:payload.length,lastModified:Date.now(),
        arrayBuffer:async()=>payload.buffer.slice(payload.byteOffset,payload.byteOffset+payload.length)}])};
    const bridge=g.window.ZmodemSentry.createZmodemBridge({ws:shim,
      term:{write:s=>{const t=s.trim();if(t.startsWith('[zmodem]'))traces.push(t);}},
      config:{maxUploadBytes:1<<30,zmodemTimeoutSec:180,zmodemBlockSize:0},checkModemState(){},flashLed(){}});
    ws.on('message',d=>bridge.consume(new Uint8Array(d)));
    ws.on('close',(c,r)=>closes.push('ws-closed code='+c+' '+String(r||'')));

    // Drive the modem: dial, then trigger the upload.
    await sleep(600);
    if(!NO_GW) ws.send(Buffer.from('ATDT555-1212\r'));
    const done=new Promise(r=>{rzDone=r;});
    // Watch the hops. Freezing is the symptom; this says where.
    let last=-1, stalledFor=0;
    const watch=setInterval(()=>{
      const line=`      t+${((Date.now()-runStart)/1000).toFixed(0)}s bridge-sent=${sentBytes} bbs-in=${bbsBytes} `+
        `wsBuffered=${ws.bufferedAmount} rz-out=${rzOutBytes}`;
      if(bbsBytes===last){ stalledFor+=2; if(stalledFor===4||stalledFor%20===0) console.log(line+'  <-- STALLED '+stalledFor+'s'); }
      else { stalledFor=0; if(process.env.VERBOSE) console.log(line); }
      last=bbsBytes;
    },2000);
    const code=await Promise.race([done,sleep(300000).then(()=>'TIMEOUT')]);
    clearInterval(watch);
    const landed=path.join(drop,'Simcity 2000.zip');
    const got=fs.existsSync(landed)?fs.statSync(landed).size:-1;
    const ok=got===SIZE&&fs.readFileSync(landed).equals(payload);
    if(!ok||code!==0) { failures++;
      console.log(`run ${run}: FAIL rz=${code} bytes=${got}/${SIZE} events=${JSON.stringify(events)}`);
      for(const t of traces.slice(-8)) console.log('    '+t.slice(0,110));
      console.log(`    final: bridge-sent=${sentBytes} bbs-in=${bbsBytes} rz-out=${rzOutBytes} wsBuffered=${ws.bufferedAmount}`);
      console.log('    closes: '+JSON.stringify(closes));
      const lines=rzErr.split(/[\r\n]+/).filter(l=>/Retry|Bad|TIMEOUT|Garbage|error|removed|Caught|too long/i.test(l));
      console.log('    rz diagnostics ('+lines.length+'): '+lines.slice(0,12).join(' / ').slice(0,700));
      console.log('    gateway: '+gwlog.split('\n').filter(l=>/error|closed|timeout|NO CARRIER/i.test(l)).slice(-3).join(' | ').slice(0,400));
    } else console.log(`run ${run}: ok (${got} bytes, identical)`);
    try{fs.unlinkSync(landed);}catch(_){}
    ws.close();
    if(rzProc){ rzProc.kill(); rzProc=null; }
    rzDone=null; rzErr=''; closes=[];
    await sleep(1500); // let the gateway drop its side before the next dial
  }
  console.log(`\n${RUNS-failures}/${RUNS} runs identical`);
  const iac=gwlog.split('\n').filter(l=>l.includes('outbound_iac'));
  console.log('gateway IAC verdict: '+(iac[0]||'(no outbound_iac line)'));
  if(gw) gw.kill(); proxy.kill(); bbs.close(); if(rzProc) rzProc.kill();
  fs.rmSync(drop,{recursive:true,force:true});
  process.exit(failures?1:0);
})().catch(e=>{console.error(e);process.exit(1);});
