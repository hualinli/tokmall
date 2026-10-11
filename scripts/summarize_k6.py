"""Archive compact k6 baseline evidence; retain full event streams at their raw path."""
import argparse
import json
from pathlib import Path
import shutil
import statistics
import re

from benchmark_backend import values


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('raw', type=Path)
    parser.add_argument('archive', type=Path)
    args = parser.parse_args()
    args.archive.mkdir(parents=True, exist_ok=True)
    shutil.copy2(args.raw/'environment.json', args.archive/'environment.json')
    rows = []
    for path in sorted(args.raw.glob('*/summary.json')):
        s = json.loads(path.read_text())
        m = s['metrics']
        commerce = 'successful_transactions' in m
        requests = m['http_reqs']['values']
        latency = m['transaction_duration' if commerce else 'feed_duration']['values']
        row = dict(case=path.parent.name, vus=s['vus'], duration_seconds=s['duration_seconds'],
                   requests=requests['count'], request_rate=requests['rate'],
                   http_error_rate=m['http_req_failed']['values']['rate'],
                   check_failures=m['checks']['values']['fails'], p95_ms=latency['p(95)'],
                   p99_ms=latency['p(99)'], k6_exit_code=s['k6_exit_code'])
        if commerce:
            row.update(transaction_rate=m['successful_transactions']['values']['rate'],
                       transactions=m['successful_transactions']['values']['count'],
                       facts=s['facts'], reconcile_exit_code=s['reconcile_exit_code'])
            f = s['facts']
            row['measurement_matches_facts'] = (
                f['paid'] == row['transactions'] == f['payments'] and
                f['inventory'] == 1000000-f['orders'] == f['ledger'] and f['negative'] == 0)
            row['closed_loop_passed'] = (s['k6_exit_code'] == s['reconcile_exit_code'] == 0 and
                row['measurement_matches_facts'] and f['projected_orders'] == f['paid'] and
                f['missing_receipts'] == f['due'] == f['dead'] == 0)
            for name in ['create_order_duration','pay_order_duration']:
                if name in m:
                    row[name] = m[name]['values']
        before = values((path.parent/'metrics-before.prom').read_text())
        after = values((path.parent/'metrics-after.prom').read_text())
        row['pool_wait_count'] = sum(v-before.get(k,0) for k,v in after.items() if k.startswith('db_pool_wait_count_total'))
        row['pool_wait_seconds'] = sum(v-before.get(k,0) for k,v in after.items() if k.startswith('db_pool_wait_duration_seconds'))
        samples = [json.loads(x) for x in (path.parent/'samples.jsonl').read_text().splitlines()]
        first,last = samples[0],samples[-1]
        span = last['elapsed']-first['elapsed']
        row['processes'] = {}
        for role,pid in zip(['api','k6'],first['processes']):
            retained = [x for x in samples if pid in x['processes']]
            final = retained[-1]
            seconds = final['elapsed']-first['elapsed']
            row['processes'][role] = dict(cpu_percent_sampled=(final['processes'][pid]['cpu_seconds']-
                first['processes'][pid]['cpu_seconds'])/seconds*100 if seconds else None,
                peak_rss_mib=max(x['processes'][pid]['rss_kib'] for x in retained)/1024)
        row['sampled_seconds'] = span
        rows.append(row)
        target = args.archive/path.parent.name
        target.mkdir(exist_ok=True)
        for name in ['summary.json','k6-summary.json','metrics-before.prom','metrics-after.prom',
                     'database-before.json','database-after.json','samples.jsonl','reconcile.json',
                     'reconcile.log','k6.log']:
            source = path.parent/name
            if source.exists():
                shutil.copy2(source, target/name)
    confirm = [r for r in rows if r['case'].startswith('feed-confirm')]
    report = dict(cases=rows, raw_directory=str(args.raw),
                  feed_confirmation=dict(rounds=len(confirm),
                    zero_errors=bool(confirm) and all(r['k6_exit_code']==0 for r in confirm)))
    for key in ['request_rate','p95_ms','p99_ms']:
        vals = [r[key] for r in confirm]
        if vals:
            report['feed_confirmation'][key] = dict(median=statistics.median(vals),min=min(vals),max=max(vals))
    commerce_confirm = [r for r in rows if r['case'].startswith('commerce-confirm')]
    report['commerce_confirmation'] = dict(rounds=len(commerce_confirm),
        all_passed=bool(commerce_confirm) and all(r['closed_loop_passed'] for r in commerce_confirm),
        total_orders=sum(r['transactions'] for r in commerce_confirm))
    for key in ['transaction_rate','p95_ms','p99_ms']:
        vals = [r[key] for r in commerce_confirm]
        if vals:
            report['commerce_confirmation'][key] = dict(median=statistics.median(vals),min=min(vals),max=max(vals))
    groups = {}
    for row in rows:
        name = re.sub(r'-r\d+$','',row['case'])
        groups.setdefault(name,[]).append(row)
    report['stage_groups'] = {}
    for name, members in groups.items():
        group = dict(rounds=len(members), all_passed=all(r.get('closed_loop_passed',r['k6_exit_code']==0) for r in members))
        rate_key = 'transaction_rate' if 'transaction_rate' in members[0] else 'request_rate'
        for key in [rate_key,'p95_ms','p99_ms']:
            vals=[r[key] for r in members]
            group[key]=dict(median=statistics.median(vals),min=min(vals),max=max(vals))
        report['stage_groups'][name]=group
    (args.archive/'aggregate.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report,indent=2))


if __name__ == '__main__':
    main()
