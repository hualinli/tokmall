"""Current full-cache baseline in a newly created, isolated PostgreSQL database.

Uses Redis DB15 only after confirming it is empty. Raw results stay under .gocache.
Each commerce case includes real video attribution and all normal background workers.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import urllib.request

import benchmark_backend as b


def request(path, body=None, token=None):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(b.BASE + path, headers=headers,
                                 data=json.dumps(body).encode() if body is not None else None)
    with urllib.request.urlopen(req, timeout=30) as response:
        return json.load(response)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--resume', action='store_true')
    parser.add_argument('--confirm-commerce', action='store_true',
                        help='Repeat the best consistent commerce stage for 60 seconds, up to three rounds')
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=args.resume)
    if not args.resume and b.call(['docker', 'exec', 'tokmall-redis', 'redis-cli', '-n', '15', 'DBSIZE']) != '0':
        raise RuntimeError('Redis DB15 must be empty before this experiment')
    stamp = str(int(time.time()))
    db, bucket = 'tokmall_bench_' + stamp, 'tokmall-bench-' + stamp
    env = b.environment(db, bucket, 'C')
    env.update(DB_HOST='127.0.0.1', DB_PORT='5432', DB_USER='tokmall',
               DB_PASSWORD='tokmall', DB_SSL_MODE='disable', REDIS_ADDR='127.0.0.1:6379')
    info = dict(database=db, bucket=bucket, redis_database=15, git_head=b.call(['git', 'rev-parse', 'HEAD']),
                created_at=b.utc(), go=b.call(['go', 'version']), k6=b.call([str(b.ROOT/'bin/k6'), 'version']),
                hardware=b.call(['sysctl','-n','hw.model','hw.ncpu','hw.memsize']).splitlines(),
                docker=b.call(['docker','info','--format','{{.ServerVersion}} {{.NCPU}} {{.MemTotal}}']),
                binaries={n:hashlib.sha256((b.ROOT/'bin'/n).read_bytes()).hexdigest() for n in ['api','reconcile','k6']},
                method='closed-loop k6; load generator and containers share host; caches L1+L2 on; rate limit off; default workers and logging on; pool 25; synthetic media; attributed simulated payments')
    if args.resume:
        previous = json.loads((output/'environment.json').read_text())
        if previous['binaries'] != info['binaries'] or previous['git_head'] != info['git_head']:
            raise RuntimeError('Cannot resume with changed service/tool binaries or Git HEAD')
        info = previous
        db = info['database']
        if not db.startswith('tokmall_bench_') or info['redis_database'] != 15:
            raise RuntimeError('Invalid isolated experiment resources')
        stamp = db.removeprefix('tokmall_bench_')
        bucket = 'tokmall-bench-' + stamp
        env.update(DB_DATABASE=db, S3_BUCKET=bucket)
    else:
        (output/'environment.json').write_text(json.dumps(info, indent=2))
        b.call(['docker','exec','tokmall-postgres','createdb','-U','tokmall',db])
        b.sql(db, 'ALTER DATABASE ' + db + ' SET track_io_timing=on')
    work = Path(tempfile.mkdtemp(prefix='tokmall-k6-'))
    if not args.resume:
        api = b.start_api(env, work/'bootstrap', output/'bootstrap.log')
        b.stop(api)
        with (output/'seed.log').open('w') as log:
            subprocess.run(['docker','exec','-i','tokmall-postgres','psql','-U','tokmall','-d',db,'-v','ON_ERROR_STOP=1'],
                           input=(b.ROOT/'scripts/seed_perf.sql').read_text(), text=True, stdout=log, check=True)
        info['dataset'] = json.loads(b.sql(db, "SELECT json_build_object('users',(SELECT count(*) FROM users),'videos',(SELECT count(*) FROM videos),'bytes',pg_database_size(current_database()))"))
        (output/'environment.json').write_text(json.dumps(info, indent=2))
    print('SEEDED', db, info['dataset'], flush=True)

    def case(name, scenario, vus, seconds, sku_count=1):
        nonlocal api
        folder = output/name
        if (folder/'summary.json').exists():
            print('SKIP', name, flush=True)
            return json.loads((folder/'summary.json').read_text())
        folder.mkdir(exist_ok=True)
        if scenario == 'commerce':
            b.sql(db, "DELETE FROM videos WHERE title='k6 attribution' AND object_key='videos/seed/k6.mp4'")
            b.reset_commerce(db)
        b.call(['docker','exec','tokmall-redis','redis-cli','-n','15','FLUSHDB'])
        api = b.start_api(env, work/name, folder/'api.log')
        generator = None
        try:
            fixture = dict(run=name, buyers=[], skus=[])
            if scenario == 'commerce':
                def account(label):
                    return request('/api/v1/auth/register', dict(email=f'{label}{stamp}{name}@load.test',
                        username='u'+hashlib.sha256((label+name+stamp).encode()).hexdigest()[:14], password='password123'))
                merchant = account('merchant')
                token = merchant['access_token']
                fixture['author_id'] = merchant['user']['user_id']
                for i in range(sku_count):
                    product = request('/api/v1/products', {'title':f'k6 product {i}'}, token)
                    pid = product['product_id']
                    sku = request(f'/api/v1/products/{pid}/skus', {'spec':'default','price':100}, token)
                    request(f'/api/v1/products/{pid}/skus/{sku["sku_id"]}/stock',
                            {'change':1000000//sku_count,'idempotency_key':f'stock-{name}-{i}'}, token)
                    video_id = int(b.sql(db, "INSERT INTO videos (author_id,title,object_key,duration_seconds,status,published_at,created_at,updated_at,media_probe_status) VALUES (" +
                        str(fixture['author_id']) + ", 'k6 attribution','videos/seed/k6.mp4',30,'PUBLISHED',now(),now(),now(),'SKIPPED') RETURNING id" ).splitlines()[0])
                    request(f'/api/v1/videos/{video_id}/products', {'product_id':pid}, token)
                    fixture['skus'].append(dict(sku_id=sku['sku_id'],product_id=pid,video_id=video_id))
                fixture['buyers'] = [account(f'buyer{i}') for i in range(vus)]
                # Tokens are local temporary test credentials; never placed in the report.
            else:
                for _ in range(100):
                    if len(request('/api/v1/feed?limit=20')['items']) != 20:
                        raise RuntimeError('invalid feed warmup')
                    time.sleep(.05)
            fixture_path = work/(name+'.json')
            fixture_path.write_text(json.dumps(fixture))
            kenv = dict(os.environ, FIXTURE=str(fixture_path), SUMMARY=str(folder/'k6-summary.json'),
                        BASE=b.BASE, SCENARIO=scenario, VUS=str(vus), DURATION=f'{seconds}s')
            (folder/'metrics-before.prom').write_text(b.metrics())
            (folder/'database-before.json').write_text(json.dumps(b.dbstats(db)))
            print('START', name, seconds, 'seconds', flush=True)
            began = time.monotonic()
            with (folder/'k6.log').open('w') as log, (folder/'samples.jsonl').open('w') as samples:
                generator = subprocess.Popen([str(b.ROOT/'bin/k6'),'run','--quiet','--out',f'json={folder}/samples-k6.json.gz',
                    str(b.ROOT/'scripts/k6-baseline.js')], env=kenv, stdout=log, stderr=subprocess.STDOUT)
                while generator.poll() is None:
                    samples.write(json.dumps(dict(elapsed=time.monotonic()-began,metrics=b.values(b.metrics()),
                        processes=b.process_rows([api.pid,generator.pid])))+'\n')
                    samples.flush()
                    time.sleep(5)
            summary = json.loads((folder/'k6-summary.json').read_text())
            summary['k6_exit_code'] = generator.returncode
            summary['vus'] = vus
            summary['duration_seconds'] = seconds
            (folder/'metrics-after.prom').write_text(b.metrics())
            (folder/'database-after.json').write_text(json.dumps(b.dbstats(db)))
            if scenario == 'commerce':
                # Observe normal delivery/consumption, without manual event replay.
                for _ in range(12):
                    missing = int(b.sql(db,"SELECT count(*) FROM outbox_events e WHERE e.status='PUBLISHED' AND NOT EXISTS (SELECT 1 FROM consumed_events c WHERE c.event_id=e.event_id)"))
                    due = int(b.sql(db,"SELECT count(*) FROM outbox_events WHERE status='PENDING' AND available_at<=now()"))
                    if missing == 0 and due == 0:
                        break
                    time.sleep(5)
                facts = json.loads(b.sql(db,"""SELECT json_build_object(
                  'orders',(SELECT count(*) FROM orders),'paid',(SELECT count(*) FROM orders WHERE paid_at IS NOT NULL),
                  'payments',(SELECT count(*) FROM payments),'inventory',(SELECT sum(quantity) FROM inventory),
                  'ledger',(SELECT sum(change) FROM inventory_transactions),'negative',(SELECT count(*) FROM inventory WHERE quantity<0),
                  'due',(SELECT count(*) FROM outbox_events WHERE status='PENDING' AND available_at<=now()),
                  'dead',(SELECT count(*) FROM outbox_events WHERE status='DEAD'),
                  'missing_receipts',(SELECT count(*) FROM outbox_events e WHERE status='PUBLISHED' AND NOT EXISTS (SELECT 1 FROM consumed_events c WHERE c.event_id=e.event_id)),
                  'projected_orders',(SELECT coalesce(sum(order_count),0) FROM author_sales_daily))"""))
                reconcile = subprocess.run([str(b.ROOT/'bin/reconcile'),'--json','--grace','0s'], env=env,text=True,capture_output=True)
                (folder/'reconcile.json').write_text(reconcile.stdout)
                (folder/'reconcile.log').write_text(reconcile.stderr)
                summary['facts'] = facts
                summary['reconcile_exit_code'] = reconcile.returncode
            (folder/'summary.json').write_text(json.dumps(summary, indent=2))
            metrics = summary['metrics']
            throughput = metrics.get('successful_transactions',metrics['http_reqs'])['values']['rate']
            latency = metrics['transaction_duration' if scenario=='commerce' else 'feed_duration']['values']
            print('DONE',name,'rate=',round(throughput,1),'p95=',round(latency['p(95)'],2),
                  'p99=',round(latency['p(99)'],2),'exit=',generator.returncode,'facts=',summary.get('facts'),flush=True)
            return summary
        finally:
            if generator is not None and generator.poll() is None:
                b.stop(generator)
            b.stop(api)

    try:
        feed = []
        for vus in [10,50,100,200,400]:
            feed.append(case(f'feed-c{vus}','feed',vus,30))
        eligible_feed = [s for s in feed if s['k6_exit_code'] == 0]
        if not eligible_feed:
            raise RuntimeError('No zero-error Feed stage available for confirmation')
        best = max(eligible_feed, key=lambda s:s['metrics']['http_reqs']['values']['rate'])['vus']
        for round_no in range(1,4):
            case(f'feed-confirm-c{best}-r{round_no}','feed',best,60)
        for skus in [1,100]:
            consistent = []
            for vus in [10,50,100]:
                result = case(f'commerce-skus{skus}-c{vus}','commerce',vus,30,skus)
                if result['k6_exit_code'] or result['reconcile_exit_code']:
                    print('CONSISTENCY/ERROR LIMIT', skus, vus, flush=True)
                    break
                consistent.append(result)
            if args.confirm_commerce and consistent:
                candidate = max(consistent, key=lambda s:s['metrics']['successful_transactions']['values']['rate'])['vus']
                for round_no in range(1,4):
                    result = case(f'commerce-confirm-skus{skus}-c{candidate}-r{round_no}',
                                  'commerce',candidate,60,skus)
                    if result['k6_exit_code'] or result['reconcile_exit_code']:
                        print('CONFIRMATION FAILED',skus,candidate,flush=True)
                        break
    finally:
        import shutil
        shutil.rmtree(work)
        print('RESULTS', output, flush=True)


if __name__ == '__main__':
    main()
