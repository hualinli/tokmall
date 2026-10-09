#!/usr/bin/env python3
"""Serial, isolated HTTP benchmarks. Never switches off transaction guarantees."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time
import urllib.request
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parent.parent
BASE = 'http://127.0.0.1:18080'
METRICS = 'http://127.0.0.1:19091/metrics'


def call(args, **kwargs):
    return subprocess.check_output(args, text=True, **kwargs).strip()


def sql(db, query):
    return call(['docker', 'exec', 'tokmall-postgres', 'psql', '-U', 'tokmall', '-d', db,
                 '-v', 'ON_ERROR_STOP=1', '-tA', '-c', query])


def utc():
    return datetime.now(timezone.utc).isoformat()


def fetch(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return response.read().decode()


def metrics():
    return fetch(METRICS)


def values(raw):
    parsed = {}
    for line in raw.splitlines():
        if not line or line.startswith('#'):
            continue
        name, value = line.rsplit(' ', 1)
        try:
            parsed[name] = float(value)
        except ValueError:
            pass
    return parsed


def dbstats(db):
    query = """SELECT json_build_object('database',
       (SELECT row_to_json(d) FROM (SELECT xact_commit,xact_rollback,blks_read,blks_hit,
        tup_returned,tup_fetched,tup_inserted,tup_updated,tup_deleted,deadlocks,
        blk_read_time,blk_write_time FROM pg_stat_database WHERE datname=current_database())d),
       'tables',(SELECT json_agg(row_to_json(t)) FROM
        (SELECT relname,seq_scan,idx_scan,n_tup_ins,n_tup_upd,n_tup_del,n_live_tup
         FROM pg_stat_user_tables ORDER BY relname)t))"""
    return json.loads(sql(db, query))


def process_rows(pids):
    raw = call(['ps','-o','pid=,pcpu=,rss=,time=','-p',','.join(str(p) for p in pids)])
    rows = {}
    for line in raw.splitlines():
        pid,percent,rss,cpu = line.split()
        seconds = sum(float(v) * (60 ** i) for i,v in enumerate(reversed(cpu.split(':'))))
        rows[int(pid)] = dict(cpu_seconds=seconds,cpu_percent=float(percent),rss_kib=int(rss))
    return rows


def stop(proc):
    if proc.poll() is None:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


def environment(db, bucket, cache='C'):
    return dict(os.environ, APP_ENV='development', SERVER_PORT='18080',
                METRICS_ADDR='127.0.0.1:19091', DB_DATABASE=db, REDIS_DB='15',
                JWT_SECRET='benchmark-local-jwt-only', FEED_CURSOR_SECRET='benchmark-local-cursor-only',
                MINIO_BUCKET=bucket, RATE_LIMIT_ENABLED='false',
                FEED_CACHE_ENABLED=str(cache != 'A').lower(),
                FEED_L1_ENABLED=str(cache == 'C').lower(),
                OUTBOX_STREAM='tokmall:benchmark:events', RECONCILE_ENABLED='true')


def start_api(env, work, stderr_path):
    work.mkdir(parents=True, exist_ok=True)
    (work / 'migrations').symlink_to(ROOT / 'migrations', target_is_directory=True)
    errfile = stderr_path.open('w')
    proc = subprocess.Popen([str(ROOT / 'bin/api')], env=env, cwd=work,
                            stdout=subprocess.DEVNULL, stderr=errfile)
    errfile.close()
    try:
        for _ in range(150):
            if proc.poll() is not None:
                raise RuntimeError(f'API exited: {stderr_path.read_text()}')
            try:
                fetch(BASE + '/health/ready')
                metrics()
                return proc
            except Exception:
                time.sleep(.2)
        raise RuntimeError('API/metrics readiness timeout')
    except BaseException:
        stop(proc)
        raise


def reset_commerce(db):
    sql(db, """TRUNCATE products,skus,inventory,inventory_transactions,orders,order_items,
       payments,refunds,video_products,coupons,user_coupons,outbox_events,consumed_events,
       author_sales_daily,object_delete_jobs RESTART IDENTITY CASCADE;
       DELETE FROM users WHERE email LIKE '%@load.test';""")


def run_case(output, db, bucket, workroot, name, scenario, duration, concurrency,
             cache='C', skus=1, warmup=0):
    folder = output / name
    if (folder / 'summary.json').exists():
        print('SKIP', name, flush=True)
        return
    folder.mkdir(parents=True, exist_ok=True)
    if scenario == 'commerce':
        reset_commerce(db)
    call(['docker', 'exec', 'tokmall-redis', 'redis-cli', '-n', '15', 'FLUSHDB'])
    env = environment(db, bucket, cache)
    safe = {k: v for k, v in env.items() if k in ['APP_ENV','SERVER_PORT','METRICS_ADDR',
          'DB_DATABASE','REDIS_DB','MINIO_BUCKET','RATE_LIMIT_ENABLED','FEED_CACHE_ENABLED',
          'FEED_L1_ENABLED','OUTBOX_STREAM','RECONCILE_ENABLED']}
    (folder / 'configuration.json').write_text(json.dumps(safe, indent=2))
    api = start_api(env, workroot / name, folder / 'api-stderr.log')
    generator = None
    try:
        if warmup:
            until = time.monotonic() + warmup
            while time.monotonic() < until:
                data = json.loads(fetch(BASE + '/api/v1/feed?limit=20'))
                if len(data.get('items', [])) != 20:
                    raise RuntimeError('Feed payload is not a populated 20-item page')
                time.sleep(.05)
        elif scenario == 'commerce':
            time.sleep(5)  # Allow startup existence rebuild to leave the connection pool.
        ready, gate = folder / 'ready', folder / 'start'
        ready.unlink(missing_ok=True)
        gate.unlink(missing_ok=True)
        args = [str(ROOT / 'bin/loadgen'), '-base', BASE, '-scenario', scenario,
                '-concurrency', str(concurrency), '-duration', f'{duration}s',
                '-skus', str(skus), '-stock', str(1_000_000 // skus),
                '-output', str(folder / 'loadgen.json'), '-ready-file', str(ready),
                '-start-file', str(gate)]
        with (folder / 'loadgen.log').open('w') as log:
            generator = subprocess.Popen(args, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            limit = time.monotonic() + 100
            while not ready.exists():
                if generator.poll() is not None:
                    raise RuntimeError((folder / 'loadgen.log').read_text())
                if time.monotonic() > limit:
                    raise RuntimeError('Load generator setup timeout')
                time.sleep(.1)
            before = metrics()
            (folder / 'metrics-before.prom').write_text(before)
            (folder / 'database-before.json').write_text(json.dumps(dbstats(db), indent=2))
            proc_before = process_rows([api.pid,generator.pid])
            gate.write_text('start')
            print('START', name, 'seconds=', duration, 'clients=', concurrency, 'skus=', skus, flush=True)
            began = time.monotonic()
            next_note = began + 30
            with (folder / 'samples.jsonl').open('w') as samples:
                while generator.poll() is None:
                    raw = metrics()
                    current = values(raw)
                    ps = process_rows([api.pid,generator.pid])
                    item = dict(timestamp=utc(), elapsed=time.monotonic()-began,
                                processes=ps, metrics=current)
                    samples.write(json.dumps(item) + '\n')
                    samples.flush()
                    now = time.monotonic()
                    if now >= next_note:
                        print('PROGRESS', name, f'{now-began:.0f}s',
                              'db_wait=', current.get('db_pool_wait_count_total{db="tokmall"}', 'n/a'), flush=True)
                        item['containers'] = call(['docker', 'stats', '--no-stream', '--format',
                            '{{.Name}} {{.CPUPerc}} {{.MemUsage}}', 'tokmall-postgres','tokmall-redis','tokmall-minio'])
                        (folder / f'containers-{int(now-began)}.txt').write_text(item['containers'])
                        next_note = now + 30
                    time.sleep(5)
            if generator.returncode:
                raise RuntimeError((folder / 'loadgen.log').read_text())
        after = metrics()
        (folder / 'metrics-after.prom').write_text(after)
        (folder / 'database-after.json').write_text(json.dumps(dbstats(db), indent=2))
        raw_report = json.loads((folder / 'loadgen.json').read_text())
        # Small summary keeps raw per-request latency samples in the adjacent JSON.
        report = {k: v for k, v in raw_report.items() if k not in ['request_latency','successful_transaction_latency','stages']}
        for k in ['request_latency','successful_transaction_latency']:
            report[k] = {a: b for a, b in raw_report[k].items() if a != 'samples_us'}
        report['stages'] = {stage: dict(success=v['success'], failures=v['failures'],
               latency={a:b for a,b in v['latency'].items() if a!='samples_us'})
               for stage,v in raw_report['stages'].items()}
        old, new = values(before), values(after)
        report['metric_deltas'] = {k: new[k] - old.get(k, 0) for k in new
             if k.endswith('_total') or '_total{' in k or k.startswith('db_pool_wait_duration_seconds')}
        proc_after = process_rows([api.pid])
        report['api_cpu_seconds'] = proc_after[api.pid]['cpu_seconds'] - proc_before[api.pid]['cpu_seconds']
        report['api_cpu_percent_average'] = report['api_cpu_seconds'] / report['elapsed_seconds'] * 100
        snapshots = [json.loads(line) for line in (folder/'samples.jsonl').read_text().splitlines()]
        report['api_peak_rss_mib'] = max(x['processes'][str(api.pid)]['rss_kib'] for x in snapshots)/1024
        last = snapshots[-1]
        last_client = last['processes'].get(str(generator.pid))
        if last_client and last['elapsed'] > 0:
            report['client_cpu_percent_sampled'] = (last_client['cpu_seconds'] - proc_before[generator.pid]['cpu_seconds'])/last['elapsed']*100
            report['client_peak_rss_mib'] = max(x['processes'][str(generator.pid)]['rss_kib'] for x in snapshots if str(generator.pid) in x['processes'])/1024
        report['cache_group'] = cache
        if scenario == 'commerce':
            for _ in range(30):
                pending = int(sql(db,"SELECT count(*) FROM outbox_events WHERE status='PENDING' AND available_at<=now()"))
                if pending == 0:
                    break
                time.sleep(1)
            facts = json.loads(sql(db,"""SELECT json_build_object(
             'orders',(SELECT count(*) FROM orders),
             'paid_orders',(SELECT count(*) FROM orders WHERE paid_at IS NOT NULL),
             'payment_rows',(SELECT count(*) FROM payments),
             'inventory_quantity',(SELECT sum(quantity) FROM inventory),
             'inventory_ledger_sum',(SELECT sum(change) FROM inventory_transactions),
             'inventory_negative',(SELECT count(*) FROM inventory WHERE quantity<0),
             'outbox_due',(SELECT count(*) FROM outbox_events WHERE status='PENDING' AND available_at<=now()),
             'outbox_delayed',(SELECT count(*) FROM outbox_events WHERE status='PENDING' AND available_at>now()),
             'outbox_dead',(SELECT count(*) FROM outbox_events WHERE status='DEAD'))"""))
            report['database_facts'] = facts
            if facts['paid_orders'] != report['successful_transactions'] or facts['inventory_negative']:
                (folder / 'failed-summary.json').write_text(json.dumps(report,indent=2))
                raise RuntimeError('transaction facts do not match measured successes')
            # A delayed timeout event can legitimately remain pending; projection is allowed to settle.
            reconcile = None
            for _ in range(10):
                reconcile = subprocess.run([str(ROOT / 'bin/reconcile'), '--json'],env=env,cwd=ROOT,
                             text=True,capture_output=True)
                if reconcile.returncode == 0:
                    break
                time.sleep(2)
            (folder / 'reconcile.json').write_text(reconcile.stdout)
            (folder / 'reconcile-stderr.log').write_text(reconcile.stderr)
            report['reconcile_exit_code'] = reconcile.returncode
            if reconcile.returncode:
                (folder / 'failed-summary.json').write_text(json.dumps(report,indent=2))
                raise RuntimeError('post-load reconciliation did not converge')
        (folder / 'summary.json').write_text(json.dumps(report,indent=2))
        print('DONE', name, 'success_rps=', round(report['success_rps'],1),
              'orders_s=', round(report['successful_orders_per_second'],1),
              'p99_ms=', round(report['request_latency']['p99_ms'],2),
              'errors=', report['system_failures'], flush=True)
    finally:
        if generator is not None and generator.poll() is None:
            stop(generator)
        stop(api)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output',type=Path,required=True)
    parser.add_argument('--suite',choices=['feed','commerce','all'],default='all')
    parser.add_argument('--rounds',type=int,default=3)
    parser.add_argument('--feed-seconds',type=int,default=180)
    parser.add_argument('--commerce-seconds',type=int,default=120)
    parser.add_argument('--clients',default='10,50,100')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True,exist_ok=True)
    metadata = output / 'environment.json'
    if metadata.exists():
        info = json.loads(metadata.read_text())
        for name, expected in info['binaries'].items():
            actual = hashlib.sha256((ROOT/'bin'/name).read_bytes()).hexdigest()
            if actual != expected:
                raise RuntimeError('Cannot resume with changed binary: ' + name)
    else:
        if call(['docker','exec','tokmall-redis','redis-cli','-n','15','DBSIZE']) != '0':
            raise RuntimeError('Redis DB15 is not empty; choose a dedicated empty test database')
        stamp = str(int(time.time()))
        db = 'tokmall_bench_' + stamp
        bucket = 'tokmall-bench-' + stamp
        info = dict(database=db,bucket=bucket,created_at=utc(),client_date='2026-10-09',
                    redis_database=15,git_head=call(['git','rev-parse','HEAD'],cwd=ROOT),
                    go=call(['go','version']),machine=call(['uname','-mrs']),
                    hardware=call(['sysctl','-n','hw.model','hw.ncpu','hw.memsize']).splitlines(),
                    docker=call(['docker','info','--format','{{.ServerVersion}} {{.NCPU}} {{.MemTotal}}']),
                    postgres=sql('tokmall','SELECT version()'),
                    container_limits=json.loads(call(['docker','inspect','--format',
                     '{{json .HostConfig}}','tokmall-postgres'])),
                    method='closed-loop HTTP; API, client and containers share host; pool=25; default INFO JSON logging enabled during measurement; logs rotated in temporary workdir; stdout discarded',
                    binaries={n:hashlib.sha256((ROOT/'bin'/n).read_bytes()).hexdigest() for n in ['api','loadgen','reconcile']})
        # Store only resource limits; omit mounts/other runtime configuration.
        info['container_limits']={k:info['container_limits'][k] for k in ['Memory','NanoCpus','CpuQuota','CpuPeriod']}
        metadata.write_text(json.dumps(info,indent=2))
        (output / 'working-tree.patch').write_text(call(['git','diff','--binary'],cwd=ROOT))
        with tarfile.open(output / 'source.tar.gz','w:gz') as archive:
            for parent in ['cmd','internal','migrations','scripts']:
                for path in (ROOT/parent).rglob('*'):
                    if path.is_file() and '__pycache__' not in path.parts: archive.add(path,arcname=str(path.relative_to(ROOT)))
            for n in ['go.mod','go.sum','docker-compose.yml']:
                archive.add(ROOT/n,arcname=n)
        call(['docker','exec','tokmall-postgres','createdb','-U','tokmall',db])
        sql(db,'ALTER DATABASE '+db+' SET track_io_timing=on')
        work = Path(tempfile.mkdtemp(prefix='tokmall-bench-bootstrap-'))
        api=start_api(environment(db,bucket),work,output/'bootstrap-stderr.log')
        stop(api)
        sqltext=(ROOT/'scripts/seed_perf.sql').read_text()
        subprocess.run(['docker','exec','-i','tokmall-postgres','psql','-U','tokmall','-d',db,
                        '-v','ON_ERROR_STOP=1'],input=sqltext,text=True,check=True,
                        stdout=(output/'seed.log').open('w'))
        info['dataset']=json.loads(sql(db,"SELECT json_build_object('users',(SELECT count(*) FROM users),'videos',(SELECT count(*) FROM videos),'published',(SELECT count(*) FROM videos WHERE status='PUBLISHED'),'database_bytes',pg_database_size(current_database()))"))
        metadata.write_text(json.dumps(info,indent=2))
        shutil.rmtree(work)
    db,bucket=info['database'],info['bucket']
    if not db.startswith('tokmall_bench_'): raise RuntimeError('invalid test database')
    workroot=Path(tempfile.mkdtemp(prefix='tokmall-benchmark-work-'))
    try:
        if args.suite in ['feed','all']:
            for round_no in range(1,args.rounds+1):
                order=['A','B','C'] if round_no%2 else ['C','B','A']
                for group in order:
                    run_case(output,db,bucket,workroot,f'feed-{group}-r{round_no}',
                             'feed',args.feed_seconds,50,cache=group,warmup=30)
        if args.suite in ['commerce','all']:
            for round_no in range(1,args.rounds+1):
                for clients in [int(v) for v in args.clients.split(',')]:
                    for skus in ([1,100] if round_no%2 else [100,1]):
                        run_case(output,db,bucket,workroot,
                           f'commerce-skus{skus}-c{clients}-r{round_no}','commerce',
                           args.commerce_seconds,clients,skus=skus)
    finally:
        shutil.rmtree(workroot)


if __name__=='__main__':
    main()
