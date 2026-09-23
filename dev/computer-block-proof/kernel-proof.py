"""Disposable-container kernel NBD proof. No filesystem formatting or mounts."""
import errno, fcntl, json, mmap, os, signal, socket, struct, subprocess, sys, tempfile, time

SET_SOCK, SET_BLKSIZE, SET_SIZE, DO_IT = 0xab00, 0xab01, 0xab02, 0xab03
CLEAR_SOCK, DISCONNECT, SET_TIMEOUT, SET_FLAGS = 0xab04, 0xab08, 0xab09, 0xab0a
MAGIC, OPT_MAGIC = 0x4e42444d41474943, 0x49484156454f5054
used = set()
owned = []

def recv(s, n):
    out = bytearray()
    while len(out) < n:
        b = s.recv(n-len(out))
        if not b: raise RuntimeError('short handshake')
        out += b
    return bytes(out)

def connect(path):
    deadline=time.monotonic()+10
    while True:
        s = socket.socket(socket.AF_UNIX); s.settimeout(10)
        try:
            s.connect(path)
            a,b,flags = struct.unpack('>QQH', recv(s,18))
            if (a,b) != (MAGIC,OPT_MAGIC) or not flags & 1:
                raise RuntimeError('not fixed newstyle')
            client = 1 | (flags & 2)
            s.sendall(struct.pack('>I',client))
            s.sendall(struct.pack('>QII',OPT_MAGIC,1,0))
            size, features = struct.unpack('>QH', recv(s,10))
            if not client & 2: recv(s,124)
            if size != 16*1024*1024 or not features & 4:
                raise RuntimeError('incorrect geometry or missing FLUSH')
            s.settimeout(None)
            return s,size,features
        except (FileNotFoundError,ConnectionRefusedError):
            s.close()
            if time.monotonic()>=deadline:raise
            time.sleep(.025)
        except BaseException:
            s.close();raise

class Attachment:
    def __init__(self,path):
        self.fd=None;self.child=None;self.claimed=False
        owned.append(self)
        s,size,flags=connect(path)
        try:
            for n in range(15,-1,-1):
                dev=f'/dev/nbd{n}'
                if dev in used or not os.path.exists(dev) or os.path.exists(f'/sys/block/nbd{n}/pid'): continue
                try: fd=os.open(dev,os.O_RDWR|os.O_EXCL)
                except OSError as e:
                    if e.errno==errno.EBUSY:continue
                    raise
                try: fcntl.ioctl(fd,SET_SOCK,s.fileno())
                except OSError as e:
                    os.close(fd)
                    if e.errno==errno.EBUSY: continue
                    raise
                self.fd=fd;self.claimed=True;self.dev=dev;used.add(dev);break
            if self.fd is None: raise RuntimeError('no unused NBD device could be atomically claimed')
            fcntl.ioctl(self.fd,SET_BLKSIZE,4096)
            fcntl.ioctl(self.fd,SET_SIZE,size)
            fcntl.ioctl(self.fd,SET_FLAGS,flags)
            fcntl.ioctl(self.fd,SET_TIMEOUT,10)
            self.child=os.fork()
            if self.child==0:
                signal.signal(signal.SIGTERM,signal.SIG_DFL)
                s.close()
                try:
                    fcntl.ioctl(self.fd,DO_IT)
                    os._exit(0)
                except OSError as e:
                    os._exit(0 if e.errno in (errno.EPIPE,errno.EIO,errno.ETIMEDOUT) else 1)
            for _ in range(100):
                if os.path.exists('/sys/block/'+os.path.basename(self.dev)+'/pid'): break
                pid,status=os.waitpid(self.child,os.WNOHANG)
                if pid: self.child=None;raise RuntimeError(f'NBD driver exited {status}')
                time.sleep(.01)
            else: raise RuntimeError('driver did not become ready')
        except BaseException:
            self.close();raise
        finally: s.close()
    def close(self):
        if self.fd is None:return
        errors=[]
        if self.claimed:
            try:fcntl.ioctl(self.fd,DISCONNECT)
            except OSError as e:
                if e.errno not in (errno.EINVAL,errno.ENOTCONN,errno.EIO):errors.append(str(e))
        if self.child:
            deadline=time.monotonic()+15
            while True:
                pid,status=os.waitpid(self.child,os.WNOHANG)
                if pid:
                    self.child=None
                    if status:errors.append(f'driver exit status {status}')
                    break
                if time.monotonic()>deadline:
                    errors.append('driver exceeded cleanup deadline')
                    os.kill(self.child,signal.SIGKILL)
                    # SIGKILL interrupts DO_IT; preserve state if the kernel does not reap it.
                    deadline2=time.monotonic()+3
                    while time.monotonic()<deadline2:
                        pid,status=os.waitpid(self.child,os.WNOHANG)
                        if pid:self.child=None;break
                        time.sleep(.05)
                    break
                time.sleep(.05)
        if self.child:
            raise RuntimeError('owned driver still alive; keep fd for cleanup: '+repr(errors))
        if self.claimed:
            try:fcntl.ioctl(self.fd,CLEAR_SOCK);self.claimed=False
            except OSError as e:errors.append(str(e))
        if not self.claimed:
            os.close(self.fd);self.fd=None
        if errors:raise RuntimeError('owned attachment cleanup: '+repr(errors))


def start_server(binary,root,sock,create=False):
    args=[binary,'-dir',root,'-socket',sock,'-size',str(16*1024*1024),'-limit','4096']
    if create:args.append('-create')
    p=subprocess.Popen(args)
    try:
        for _ in range(200):
            if os.path.exists(sock):return p # connect() independently verifies readiness
            if p.poll() is not None:raise RuntimeError('server exited during startup')
            time.sleep(.025)
        raise RuntimeError('server socket timeout')
    except BaseException:
        if p.poll() is None:p.kill()
        p.wait(timeout=5);raise

def main():
    if os.environ.get('HELMR_DISPOSABLE_NBD_PROOF')!='1':raise RuntimeError('disposable fixture required')
    payload=os.urandom(4096)
    with tempfile.TemporaryDirectory(prefix='helmr-nbd-') as tmp:
        root=tmp+'/state';sock=tmp+'/first.sock'
        p=start_server(sys.argv[1],root,sock,create=True);a=None;b=None;q=None
        try:
            a=Attachment(sock)
            if os.pwrite(a.fd,payload,4096)!=len(payload):raise RuntimeError('short write')
            os.fsync(a.fd)
            first=a.dev
            p.kill();p.wait() # successful kernel FLUSH precedes abrupt server death
            a.close();a=None
            q=start_server(sys.argv[1],root,tmp+'/second.sock')
            b=Attachment(tmp+'/second.sock')
            direct=os.open(b.dev,os.O_RDONLY|os.O_DIRECT)
            try:
                with mmap.mmap(-1,len(payload)) as aligned:
                    n=os.preadv(direct,[aligned],4096)
                    if n!=len(payload):raise RuntimeError('short direct read')
                    actual=aligned[:]
            finally:os.close(direct)
            if actual!=payload:raise RuntimeError('restart data mismatch')
            second=b.dev
            b.close();b=None
            report={'kernel_nbd_flush_restart':'pass','first_device':first,'second_device':second,'bytes':len(payload),'vm':False,'remote_store':False}
        finally:
            cleanup=[]
            # Closing server sockets also releases blocked kernel requests on failures.
            for proc in (q,p):
                try:
                    if proc and proc.poll() is None:proc.kill();proc.wait(timeout=5)
                except BaseException as e:cleanup.append(str(e))
            for attach in reversed(owned):
                try:
                    if attach:attach.close()
                except BaseException as e:cleanup.append(str(e))
            if cleanup:raise RuntimeError('cleanup incomplete: '+repr(cleanup))

    for dev in used:
        if os.path.exists('/sys/block/'+os.path.basename(dev)+'/pid'):raise RuntimeError('owned device still active')
    print('owned device cleanup: pass')
    print(json.dumps(report))

def interrupted(signum,frame):
    raise RuntimeError('fixture interrupted; cleaning owned resources')

if __name__=='__main__':
    signal.signal(signal.SIGTERM,interrupted)
    main()
