#!/usr/bin/env python3
"""Check that a rewording of the manuscript changed no content it must not change.

Usage: check-rewording-invariants.py OLD.tex NEW.tex
Compares, as multisets, the evidence macros, citation keys, labels and references,
\\code spans, inline math and literal numbers of the two files. A readability pass
(splitting or reordering sentences) must leave all six identical.
"""
import re,sys,collections,pathlib
def feats(path):
    t=pathlib.Path(path).read_text()
    t=re.sub(r'(?m)(?<!\\)%.*$','',t)
    f={}
    f['evidence macros']=collections.Counter(re.findall(r'\\([A-Z][A-Za-z]+)(?![A-Za-z])',t))
    cites=[]
    for m in re.finditer(r'\\cite\{([^}]*)\}',t): cites+= [k.strip() for k in m.group(1).split(',')]
    f['cite keys']=collections.Counter(cites)
    f['refs']=collections.Counter(re.findall(r'\\(?:ref|label)\{([^}]*)\}',t))
    f['code spans']=collections.Counter(re.findall(r'\\code\{([^}]*)\}',t))
    f['math']=collections.Counter(re.sub(r'\s+','',m) for m in re.findall(r'\$([^$]+)\$',t))
    body=re.sub(r'\\[A-Za-z]+',' ',t); body=re.sub(r'\$[^$]*\$',' ',body)
    f['numbers']=collections.Counter(re.findall(r'(?<![A-Za-z])\d[\d,]*(?:\.\d+)?',body))
    return f
a,b=feats(sys.argv[1]),feats(sys.argv[2]); ok=True
for k in a:
    d1=a[k]-b[k]; d2=b[k]-a[k]
    if d1 or d2:
        ok=False; print(f'{k}: removed {dict(d1)} | added {dict(d2)}')
print('INVARIANTS OK' if ok else 'INVARIANTS DIFFER')
