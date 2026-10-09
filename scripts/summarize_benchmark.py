#!/usr/bin/env python3
"""Aggregate completed rounds without choosing the fastest run."""
import argparse
import csv
import json
from pathlib import Path
from statistics import median


def aggregate(root):
    groups = {}
    for path in sorted(root.glob('*/summary.json')):
        row = json.loads(path.read_text())
        if row['scenario'] == 'feed':
            key = 'feed-' + row['cache_group']
        else:
            key = 'commerce-skus%d-c%d' % (row['sku_count'], row['concurrency'])
        groups.setdefault(key, []).append((path.parent.name, row))
    results = []
    for key, rounds in sorted(groups.items()):
        result = dict(group=key, rounds=len(rounds), cases=[name for name, _ in rounds])
        metrics = {
            'success_rps': lambda r: r['success_rps'],
            'orders_per_second': lambda r: r['successful_orders_per_second'],
            'request_p95_ms': lambda r: r['request_latency']['p95_ms'],
            'request_p99_ms': lambda r: r['request_latency']['p99_ms'],
            'transaction_p95_ms': lambda r: r['successful_transaction_latency']['p95_ms'],
            'transaction_p99_ms': lambda r: r['successful_transaction_latency']['p99_ms'],
            'api_cpu_percent': lambda r: r['api_cpu_percent_average'],
            'api_peak_rss_mib': lambda r: r['api_peak_rss_mib'],
            'db_wait_count': lambda r: r['metric_deltas'].get('db_pool_wait_count_total{db="tokmall"}', 0),
            'db_wait_seconds': lambda r: r['metric_deltas'].get('db_pool_wait_duration_seconds{db="tokmall"}', 0),
        }
        for name, extract in metrics.items():
            values = [extract(row) for _, row in rounds]
            result[name] = dict(median=median(values), min=min(values), max=max(values))
        for name in ['requests', 'success', 'business_failures', 'system_failures', 'successful_transactions']:
            result[name] = sum(row[name] for _, row in rounds)
        result['error_rate'] = (result['business_failures'] + result['system_failures']) / result['requests']
        if rounds[0][1]['scenario'] == 'commerce':
            result['all_reconciled'] = all(row.get('reconcile_exit_code') == 0 for _, row in rounds)
            result['database_facts'] = {name: row['database_facts'] for name, row in rounds}
        results.append(result)
    return results


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('root', type=Path)
    args = parser.parse_args()
    results = aggregate(args.root)
    (args.root / 'aggregate.json').write_text(json.dumps(results, indent=2))
    columns = ['group', 'rounds', 'success_rps', 'orders_per_second', 'request_p95_ms',
               'request_p99_ms', 'transaction_p95_ms', 'transaction_p99_ms', 'error_rate']
    with (args.root / 'aggregate.csv').open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=columns)
        writer.writeheader()
        for result in results:
            writer.writerow({key: round(result[key]['median'], 3) if isinstance(result[key], dict)
                             else result[key] for key in columns})
    for result in results:
        print('%s rounds=%d success_rps=%.1f orders/s=%.1f request_p99=%.2fms transaction_p99=%.2fms errors=%d' % (
            result['group'], result['rounds'], result['success_rps']['median'],
            result['orders_per_second']['median'], result['request_p99_ms']['median'],
            result['transaction_p99_ms']['median'], result['business_failures'] + result['system_failures']))


if __name__ == '__main__':
    main()
