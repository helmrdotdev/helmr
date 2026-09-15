from pathlib import Path
import json,statistics,sys
p=Path(sys.argv[1]);summary={}
for log in (p/'results').glob('*.jsonl'):
 rows=[json.loads(line) for line in log.read_text().splitlines()];by={}
 for mode in ['plain','current','reuse','record','bare','api','entry']:
  series=[r for r in rows if r['mode']==mode and not json.loads(r['argv'][-1]).get('query')]
  if not series:continue
  values=[json.loads(r['stdout'].strip().splitlines()[-1]) for r in series]
  def distribution(x):return dict(n=len(x),median=statistics.median(x),min=min(x),max=max(x))
  d={'elapsedMs':distribution([r['elapsedMs'] for r in series]),'maxRSSKiB':distribution([r['resourceUsage']['maxRSS'] for r in values]),'phase':{k:distribution([r['phase'][k] for r in values]) for k in values[0]['phase']},'warmImportMs':distribution([t for r in values for t in r['warmMs']]) if values[0]['warmMs'] else None,'metrics':{k:distribution([r['metrics'][k] for r in values]) for k in values[0]['metrics']} if values[0].get('metrics') else None,'fsRead':distribution([r['resourceUsage']['fsRead'] for r in values])}
  by[mode]=d
 by['deltaMs']={k:by[a]['elapsedMs']['median']-by[b]['elapsedMs']['median'] for k,a,b in [('observer','current','plain'),('reuseVsObserved','reuse','current'),('reuseVsIncumbent','reuse','plain')]}
 receipt=json.loads((p/'results'/(log.stem+'.cache-receipt.json')).read_text());by['cacheBytes']=receipt['sizeBytes'];by['querySamples']=[dict(elapsedMs=r['elapsedMs'],**json.loads(r['stdout'])) for r in rows if json.loads(r['argv'][-1]).get('query')]
 summary[log.stem]=by
(p/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')
for k,v in summary.items():print(k,{m:round(v[m]['elapsedMs']['median'],2) for m in ['plain','current','reuse']},v['deltaMs'],'cache',v['cacheBytes'])
