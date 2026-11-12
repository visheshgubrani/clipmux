#!/usr/bin/env python3
"""Retained local staging processes. Cloud resources are provisioned separately."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess
import time
import urllib.request
import urllib.error

ROOT = Path(__file__).resolve().parent.parent
STATE = ROOT / 'data/cloudflare-staging'
BIN = STATE / 'bin/clipmux'
ENV = dict(os.environ, DATABASE_URL='postgres://clipmux:clipmux@127.0.0.1:5445/clipmux?sslmode=disable',
           CLIPMUX_DATA_DIR=str(STATE / 'app'), CLIPMUX_LISTEN='127.0.0.1:8086',
           CLIPMUX_PUBLIC_URL='https://app-test.clipmux.com', CLIPMUX_MEDIA_URL='https://media-test.clipmux.com',
           CLIPMUX_PACKAGER=str(STATE / 'bin/packager'), CLIPMUX_SPEECH='off', CLIPMUX_LOG_FORMAT='json')
for key in ('CLIPMUX_SELF_DIAL', 'CLIPMUX_CLOUDFLARE_API', 'CLIPMUX_R2_ENDPOINT', 'CLIPMUX_UNSANDBOXED_WORKER'):
    ENV.pop(key, None)


def private(path, value):
    with open(path, 'w', opener=lambda p, flags: os.open(p, flags, 0o600)) as stream:
        stream.write(value)
    path.chmod(0o600)


def call(path, data):
    request = urllib.request.Request('http://127.0.0.1:8086' + path,
        data=json.dumps(data).encode(), headers={'Content-Type': 'application/json', 'Origin': 'http://127.0.0.1:8086'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


def running():
    path = STATE / 'serve.pid'
    if not path.exists():
        return None
    pid = int(path.read_text())
    try:
        args = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
        return pid if args[:2] == [str(BIN).encode(), b'serve'] else None
    except FileNotFoundError:
        return None


def preflight():
    path=STATE/'cloudflare-token'
    if not path.is_file() or path.stat().st_mode & 0o077:
        raise SystemExit('Token file required with permissions 0600')
    token=path.read_text().strip()
    account='49b962322ece3363f6495da349dd9d9e'
    def get(path):
        request=urllib.request.Request('https://api.cloudflare.com/client/v4'+path,
                                     headers={'Authorization':'Bearer '+token})
        with urllib.request.urlopen(request,timeout=30) as response:
            return json.load(response)['result']
    try:
        verified=get('/user/tokens/verify')
    except urllib.error.HTTPError as error:
        if error.code not in (401,403):raise
        verified=get('/accounts/'+account+'/tokens/verify')
    if verified.get('status')!='active':raise SystemExit('Cloudflare token is not active')
    subscriptions=get('/accounts/'+account+'/subscriptions')
    paid=any('workers' in (item['rate_plan']['id']+' '+item['rate_plan']['public_name']).lower()
        and 'paid' in (item['rate_plan']['id']+' '+item['rate_plan']['public_name']).lower()
        and item.get('state','').lower()!='cancelled' for item in subscriptions)
    private(STATE/'preflight.json',json.dumps({'token_active':True,'workers_paid':paid})+'\n')
    if not paid:raise SystemExit('Token active; Workers Paid subscription is required before deployment')
    print('Token active; Workers Paid verified')


def start():
    if running():
        print('Staging app already running')
        return
    log = STATE / 'serve.log'
    private(log, '')
    with log.open('a') as stream:
        proc = subprocess.Popen([str(BIN), 'serve'], cwd=ROOT, env=ENV,
                                stdout=stream, stderr=stream, start_new_session=True)
    private(STATE / 'serve.pid', str(proc.pid))
    for _ in range(120):
        if proc.poll() is not None:
            raise SystemExit('Staging app exited; inspect the private serve.log locally')
        try:
            with urllib.request.urlopen('http://127.0.0.1:8086/readyz', timeout=1) as res:
                if res.status == 200:
                    print('Staging app ready on loopback port 8086')
                    return
        except OSError:
            pass
        time.sleep(0.5)
    raise SystemExit('Staging readiness timed out')


def bootstrap():
    owner = STATE / 'owner.json'
    if not owner.exists():
        match = re.search(r'setup_token(?:=|":")([A-Z2-7]+)', (STATE / 'serve.log').read_text())
        if not match:
            raise SystemExit('No setup token in private log; owner may already exist')
        cfg = {'email': 'staging@clipmux.com', 'password': secrets.token_urlsafe(32)}
        private(owner, json.dumps(cfg))
        try:
            call('/v1/setup/owner', dict(cfg, setup_token=match[1]))
        except Exception:
            owner.unlink()
            raise SystemExit('Owner creation failed; no credentials printed') from None
    credentials = STATE / 'integration.json'
    if not credentials.exists():
        def admin(*args):
            result = subprocess.run([str(BIN), 'admin', *args], env=ENV, cwd=ROOT, capture_output=True, text=True)
            if result.returncode:
                raise SystemExit('Staging admin failed; no credentials printed')
            return result.stdout
        key = re.search(r'key:\s+(cmx_\S+)', admin('create-key', '--app', 'Cloudflare staging', '--allow-loopback', '--scopes', 'uploads:write,assets:read,assets:write'))[1]
        signing = admin('create-signing-key', '--app', 'Cloudflare staging')
        private(credentials, json.dumps({'api_key': key, 'kid': re.search(r'kid: (kid_\S+)', signing)[1],
            'private_key': signing[signing.index('-----BEGIN'):signing.index('-----END PRIVATE KEY-----')+25]+'\n'}))
    print('Private owner and integration credentials retained')


def source():
    path = STATE / 'source.mp4'
    if path.exists():
        return
    subprocess.run(['ffmpeg', '-nostdin', '-v', 'error', '-f', 'lavfi', '-i', 'testsrc2=size=1280x720:rate=25',
        '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=48000', '-t', '30', '-c:v', 'libx264', '-preset', 'veryfast',
        '-pix_fmt', 'yuv420p', '-c:a', 'aac', '-movflags', '+faststart', str(path)], check=True)
    # A valid top-level free box pads the playable MP4 to exactly three parts.
    padding = 70*1024*1024 - path.stat().st_size
    if padding < 8:
        raise SystemExit('Generated source unexpectedly exceeds fixture size')
    with path.open('ab') as stream:
        stream.write(padding.to_bytes(4, 'big') + b'free')
        stream.truncate(70*1024*1024)
    print('Playable 70 MiB multipart fixture ready')


def prepare():
    source()
    credentials = json.loads((STATE / 'integration.json').read_text())
    base = ENV['CLIPMUX_PUBLIC_URL']
    def api(path, body=None, bearer=None, method=None):
        request = urllib.request.Request(base+path, data=json.dumps(body).encode() if body is not None else None,
            headers={'Content-Type':'application/json', 'Authorization':'Bearer '+(bearer or credentials['api_key'])}, method=method)
        with urllib.request.urlopen(request, timeout=120) as res:
            return json.load(res)
    existing = STATE / 'live.json'
    if existing.exists():
        print('Retained browser fixture already prepared')
        return
    created = api('/v1/uploads', {'size':70*1024*1024,'playback_policy':'signed'})
    signed = api('/v1/uploads/'+created['upload_id']+'/parts', {'part_numbers':[1,2,3]}, created['upload_token'])
    parts=[]
    with (STATE/'source.mp4').open('rb') as stream:
        for part in signed['parts']:
            stream.seek((part['part_number']-1)*created['part_size'])
            request=urllib.request.Request(part['url'],data=stream.read(created['part_size']),method='PUT')
            with urllib.request.urlopen(request,timeout=120) as res:
                parts.append({'part_number':part['part_number'],'etag':res.headers['ETag']})
    asset=api('/v1/uploads/'+created['upload_id']+'/complete',{'parts':parts},created['upload_token'])
    for _ in range(300):
        asset=api('/v1/assets/'+created['asset_id'])
        if asset['status']=='ready':break
        if asset['status']=='errored':raise SystemExit('Staging fixture encode failed')
        time.sleep(1)
    else:raise SystemExit('Staging fixture encode timed out')
    cfg=dict(credentials, api=base, base_url=ENV['CLIPMUX_MEDIA_URL'], asset_id=asset['id'],
             playback_id=asset['playback_ids'][0]['id'], source=str(STATE/'source.mp4'))
    private(existing,json.dumps(cfg))
    print('Baseline filesystem asset ready; private live config retained')


DB_CONTAINER = 'clipmux-staging-postgres'
DB_VOLUME = 'clipmux-staging_pgdata'


def db_start():
    """Staging's own Postgres on loopback port 5445, beside dev's on 5444,
    with a named volume that db-stop never removes."""
    found = subprocess.run(['docker', 'ps', '-a', '--filter', 'name=^'+DB_CONTAINER+'$', '--format', '{{.Status}}'],
                           capture_output=True, text=True, check=True).stdout.strip()
    if not found:
        subprocess.run(['docker', 'run', '-d', '--name', DB_CONTAINER, '-p', '127.0.0.1:5445:5432',
            '-e', 'POSTGRES_USER=clipmux', '-e', 'POSTGRES_PASSWORD=clipmux', '-e', 'POSTGRES_DB=clipmux',
            '-v', DB_VOLUME+':/var/lib/postgresql', 'postgres:18-alpine'], check=True, capture_output=True)
    elif not found.startswith('Up'):
        subprocess.run(['docker', 'start', DB_CONTAINER], check=True, capture_output=True)
    for _ in range(60):
        if subprocess.run(['docker', 'exec', DB_CONTAINER, 'pg_isready', '-U', 'clipmux', '-d', 'clipmux'],
                          capture_output=True).returncode == 0:
            print('Staging Postgres ready on loopback port 5445')
            return
        time.sleep(1)
    raise SystemExit('Staging Postgres readiness timed out')


def db_stop():
    subprocess.run(['docker', 'stop', DB_CONTAINER], check=True, capture_output=True)
    print('Staging Postgres stopped; volume '+DB_VOLUME+' retained')


def tunnel_running():
    file=STATE/'tunnel.pid'
    if not file.exists():return None
    pid=int(file.read_text())
    try:args=Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
    except FileNotFoundError:return None
    return pid if args and args[0]==str(STATE/'bin/cloudflared').encode() and str(STATE/'tunnel.yml').encode() in args else None


def tunnel_start():
    if tunnel_running():
        print('Staging Tunnel connector already running')
        return
    if not (STATE/'tunnel.json').is_file() or not (STATE/'tunnel.yml').is_file():
        raise SystemExit('Provision the dedicated staging Tunnel credentials first')
    log=STATE/'tunnel.log'
    private(log,'')
    with log.open('a') as stream:
        proc=subprocess.Popen([str(STATE/'bin/cloudflared'),'tunnel','--no-autoupdate','--no-prechecks',
            '--config',str(STATE/'tunnel.yml'),'--protocol','http2','run'],cwd=ROOT,
            stdout=stream,stderr=stream,start_new_session=True)
    private(STATE/'tunnel.pid',str(proc.pid))
    for _ in range(60):
        if proc.poll() is not None:raise SystemExit('Staging Tunnel exited; inspect private tunnel.log')
        if 'Registered tunnel connection' in log.read_text():
            print('Staging Tunnel connected over HTTP/2')
            return
        time.sleep(0.5)
    raise SystemExit('Staging Tunnel registration timed out')


def tunnel_stop():
    pid=tunnel_running()
    if pid:
        os.kill(pid,signal.SIGTERM)
        for _ in range(60):
            if not tunnel_running():break
            time.sleep(0.5)
        else:raise SystemExit('Staging Tunnel is still stopping')
    print('Staging Tunnel connector stopped; credentials retained')


def stop():
    pid = running()
    if pid:
        os.kill(pid, signal.SIGTERM)
        for _ in range(120):
            if not running():
                break
            time.sleep(0.5)
        else:
            raise SystemExit('Staging app is still draining')
    print('Staging app stopped; state retained')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'db-start', 'db-stop', 'start', 'bootstrap', 'source', 'prepare', 'stop', 'tunnel-start', 'tunnel-stop'])
    args = parser.parse_args()
    os.umask(0o077)
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    try:
        globals()[args.action.replace('-', '_')]()
    except (OSError, subprocess.CalledProcessError):
        raise SystemExit('Staging operation failed; no private data printed') from None
