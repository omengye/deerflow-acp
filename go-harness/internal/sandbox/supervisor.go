package sandbox

// Adapted from deerflow/sandbox/command.py. The distro-side supervisor owns one
// command group and adopts orphan descendants. Killing wsl.exe alone is never
// accepted as evidence of guest termination. No shared distro is terminated.
const linuxSupervisor = `
import ctypes,json,os,select,signal,subprocess,sys,threading,time
cwd,deadline,marker,argv,environment=sys.argv[1:]
deadline=float(deadline); argv=json.loads(argv); environment=json.loads(environment)
child=None; state='completed'; code=None; confirmed=True; readers=[]
def copy(source,destination):
    try:
        while True:
            data=os.read(source.fileno(),65536)
            if not data: break
            destination.write(data); destination.flush()
    except (OSError,BrokenPipeError): pass
def group_alive():
    if child is None: return False
    try: os.killpg(child.pid,0); return True
    except ProcessLookupError: return False
def reap():
    while True:
        try:
            if not os.waitpid(-1,os.WNOHANG)[0]: return
        except ChildProcessError: return
def children():
    result=set()
    for task in os.listdir('/proc/self/task'):
        try:
            with open('/proc/self/task/'+task+'/children') as f: result.update(int(p) for p in f.read().split())
        except FileNotFoundError: pass
    return result
def kill_owned(pid):
    descriptor=None
    try:
        descriptor=os.pidfd_open(pid)
        with open('/proc/'+str(pid)+'/stat') as f: parent=int(f.read().rsplit(')',1)[1].split()[1])
        if parent==os.getpid(): signal.pidfd_send_signal(descriptor,signal.SIGKILL)
    except (ProcessLookupError,FileNotFoundError): pass
    finally:
        if descriptor is not None: os.close(descriptor)
try:
    if ctypes.CDLL(None).prctl(36,1,0,0,0)!=0: raise RuntimeError('Linux subreaper is unavailable')
    child=subprocess.Popen(argv,cwd=cwd,env=environment,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
    for source,target in ((child.stdout,sys.stdout.buffer),(child.stderr,sys.stderr.buffer)):
        thread=threading.Thread(target=copy,args=(source,target),daemon=True); thread.start(); readers.append(thread)
    expires=time.monotonic()+deadline
    while child.poll() is None:
        if time.monotonic()>=expires: state='timed_out'; break
        readable,_,_=select.select([sys.stdin],[],[],0.05)
        if readable:
            request=os.read(0,1024); state='timed_out' if request.startswith(b'timed_out') else 'cancelled'; break
    code=child.poll()
except Exception as error:
    print('Command supervisor: '+str(error),file=sys.stderr); state='failed'; code=127
finally:
    if child is not None:
        try:
            if group_alive(): os.killpg(child.pid,signal.SIGKILL)
        except ProcessLookupError: pass
        try:
            child.wait(timeout=1)
            if code is None: code=child.returncode
        except subprocess.TimeoutExpired: confirmed=False
        until=time.monotonic()+2
        try:
            while time.monotonic()<until:
                reap(); owned=children()
                if not owned and not group_alive(): break
                for pid in owned: kill_owned(pid)
                time.sleep(0.02)
            reap(); confirmed=confirmed and not children() and not group_alive()
        except (OSError,AttributeError,ValueError): confirmed=False
        for reader in readers: reader.join(timeout=0.3)
        confirmed=confirmed and all(not reader.is_alive() for reader in readers)
    if state=='completed' and code!=0: state='failed'
    if not confirmed: state='uncertain'
    print('\n'+marker+json.dumps({'state':state,'exitCode':code,'confirmed':confirmed}),file=sys.stderr,flush=True)
`
