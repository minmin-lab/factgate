import json, sys, subprocess, urllib.request, math, collections, unicodedata
rep = json.load(open('/home/wmm/logs/kb1/ekb4/report-v2.json'))
queries = {}
for line in open('evaluation/kb-pilot/queryset-v2/queries.jsonl', encoding='utf-8'):
    q = json.loads(line); queries[q['query_id']] = q
diffs = []
for d in rep['deployments_detail']:
    for o in d['observations']:
        if not o['identical']:
            diffs.append((d['deployment'], o))
print('differing observations', len(diffs), 'of', sum(len(d['observations']) for d in rep['deployments_detail']))
byk = collections.Counter(o['k'] for _, o in diffs); print('by k', dict(byk))
sizes = collections.Counter(len(set(o['gateway_hits']) ^ set(o['direct_hits'])) for _, o in diffs); print('symmetric difference sizes', dict(sizes))
boundary = sum(1 for _, o in diffs if o['gateway_hits'][:-1] == o['direct_hits'][:-1]); print('differ only in last position', boundary)
same_set_diff_order = sum(1 for _, o in diffs if set(o['gateway_hits']) == set(o['direct_hits'])); print('same set, different order', same_set_diff_order)
# Re-embed differing texts singly (as the gateway does) and compare with batch embedding.
def normalize(t):
    return ' '.join(unicodedata.normalize('NFC', t).casefold().split())
def embed(texts):
    req = urllib.request.Request('http://127.0.0.1:8090/embed', data=json.dumps({'texts': texts}).encode(), headers={'content-type': 'application/json'})
    return json.load(urllib.request.urlopen(req, timeout=120))['vectors']
seen = []
for _, o in diffs:
    if o['query_id'] not in seen:
        seen.append(o['query_id'])
print('distinct differing queries', len(seen))
texts = [normalize(queries[q]['text']) for q in seen]
single = [embed([t])[0] for t in texts]
batch = embed(texts)
def cos(a, b):
    return sum(x*y for x, y in zip(a, b)) / math.sqrt(sum(x*x for x in a) * sum(y*y for y in b))
dev = [1 - cos(single[i], batch[i]) for i in range(len(texts))]
print('single-vs-batch cosine distance: max %.3e mean %.3e' % (max(dev), sum(dev)/len(dev)))
exact = sum(1 for i in range(len(texts)) if single[i] == batch[i]); print('bit-identical single vs batch', exact, 'of', len(texts))
# Direct hits with the single-text vector vs gateway hits for each differing observation.
import psycopg2  # noqa
