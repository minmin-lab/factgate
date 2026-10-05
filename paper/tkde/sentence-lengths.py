#!/usr/bin/env python3
"""List the sentences of a manuscript source longer than N words.

Usage: sentence-lengths.py FILE.tex N
A rough counter for readability passes: it splits on a period followed by a capital
or a command, so run-in numbered lists count as one sentence.
"""
import re,sys,pathlib
src=pathlib.Path(sys.argv[1]).read_text().split('\n')
# body only: between \begin{abstract} and \bibliographystyle
start=next(i for i,l in enumerate(src) if '\\begin{abstract}' in l); end=next(i for i,l in enumerate(src) if '\\bibliographystyle' in l)
# join into paragraphs keeping line numbers
paras=[];cur=[];env=0
for i in range(start,end):
    l=src[i]
    if re.match(r'\s*\\begin\{(table|figure|tabular|tikzpicture|axis|equation|align|itemize|enumerate)',l): env+=1
    if env==0 and l.strip() and not l.strip().startswith('%'): cur.append((i+1,l))
    elif cur: paras.append(cur);cur=[]
    if re.match(r'\s*\\end\{(table|figure|tabular|tikzpicture|axis|equation|align|itemize|enumerate)',l): env-=1
if cur: paras.append(cur)
def words(t):
    t=re.sub(r'\\[A-Za-z]+\*?(\{[^{}]*\})?',' W ',t); t=re.sub(r'[\$\{\}~]',' ',t); return len([w for w in t.split() if re.search(r'[A-Za-z0-9]',w)])
out=[]
for p in paras:
    text=' '.join(l for _,l in p)
    # sentence split
    pos=0
    for m in list(re.finditer(r'(?<=[.])\s+(?=[A-Z\\])',text))+[None]:
        e=m.start() if m else len(text); s=text[pos:e]; 
        if m: nxt=m.end()
        # map char offset to line
        off=0;ln=p[0][0]
        for n,l in p:
            if off+len(l)+1>pos: ln=n;break
            off+=len(l)+1
        out.append((words(s),ln,s.strip())); pos=nxt if m else e
tot=len(out); longs=[o for o in out if o[0]>int(sys.argv[2])]
print('sentences',tot,'median',sorted(o[0] for o in out)[tot//2],'>60:',sum(1 for o in out if o[0]>60),'>100:',sum(1 for o in out if o[0]>100),'>45:',sum(1 for o in out if o[0]>45))
for w,ln,s in sorted(longs,key=lambda o:o[1]): print(f"L{ln:5d} {w:4d}w  {s[:95]}")
