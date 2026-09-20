#!/usr/bin/env python3
"""Compare two generated macro files (evidence.tex / p10.tex) macro by macro.

Usage: macro-diff.py OLD.tex NEW.tex [--csv out.csv]

The paper's numbers all come from \newcommand macros, so a rerun's effect on
the paper is exactly the set of macros whose value changed. Prints four
sections: changed, added, removed, and a one-line summary. Values that are
pure numbers also get the relative change, so a 0.3% timing wobble is
visually separable from a real shift. Nothing is filtered or rounded away.
"""
import argparse, pathlib, re, sys, csv

MACRO = re.compile(r'\\newcommand\{\\([A-Za-z]+)\}\{(.*)\}\s*$')

def load(path):
    out = {}
    for line in pathlib.Path(path).read_text().splitlines():
        m = MACRO.match(line.strip())
        if m:
            out[m.group(1)] = m.group(2)
    return out

NUM = re.compile(r'^-?\d+(?:\.\d+)?$')
def numeric(v):
    v = v.replace('{', '').replace('}', '').replace('\\texttt', '').replace(',', '').strip()
    return float(v) if NUM.match(v) else None

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('old'); ap.add_argument('new'); ap.add_argument('--csv')
    a = ap.parse_args()
    old, new = load(a.old), load(a.new)
    changed = [(k, old[k], new[k]) for k in sorted(old) if k in new and old[k] != new[k]]
    added = sorted(set(new) - set(old))
    removed = sorted(set(old) - set(new))
    print(f"old={a.old} ({len(old)} macros)  new={a.new} ({len(new)} macros)\n")
    if changed:
        print(f"CHANGED ({len(changed)}):")
        w = max(len(k) for k, _, _ in changed)
        for k, o, n in changed:
            fo, fn = numeric(o), numeric(n)
            delta = ''
            if fo is not None and fn is not None and fo != 0:
                delta = f"   ({(fn-fo)/abs(fo)*100:+.1f}%)"
            print(f"  {k:<{w}}  {o}  ->  {n}{delta}")
    else:
        print("CHANGED (0): every macro kept its value")
    if added:   print(f"\nADDED ({len(added)}): " + ', '.join(added))
    if removed: print(f"\nREMOVED ({len(removed)}): " + ', '.join(removed))
    print(f"\nSUMMARY changed={len(changed)} added={len(added)} removed={len(removed)} unchanged={len(set(old)&set(new))-len(changed)}")
    if a.csv:
        with open(a.csv, 'w', newline='') as fh:
            w = csv.writer(fh); w.writerow(['macro', 'old', 'new', 'pct_change'])
            for k, o, n in changed:
                fo, fn = numeric(o), numeric(n)
                w.writerow([k, o, n, f"{(fn-fo)/abs(fo)*100:+.2f}" if fo not in (None, 0) and fn is not None else ''])
            for k in added:   w.writerow([k, '', new[k], 'ADDED'])
            for k in removed: w.writerow([k, old[k], '', 'REMOVED'])
        print(f"csv written: {a.csv}")
    return 0

sys.exit(main())
