"""Diagnose pinned model score distribution with a synthetic fixture corpus."""
import json,math,subprocess,urllib.request
from pathlib import Path
root=Path('/home/gen/deerflow-deploy/single-user-memory-20261027')
corpus=json.loads((root/'corpus.json').read_text())
info=json.loads(subprocess.check_output(['docker','inspect','deerflow-memory-embedding'],text=True))[0]
host=info['NetworkSettings']['Networks']['deerflow-deploy_default']['IPAddress']
def embed(texts):
 out=[]
 for i in range(0,len(texts),24):
  body=json.dumps({'model':'BAAI/bge-small-zh-v1.5@46fbe35','input':texts[i:i+24],'query':False}).encode()
  req=urllib.request.Request('http://'+host+':8080/embeddings',data=body,headers={'Content-Type':'application/json'})
  out+=json.load(urllib.request.urlopen(req,timeout=20))['vectors']
 return out
def cosine(a,b):return sum(x*y for x,y in zip(a,b))/math.sqrt(sum(x*x for x in a)*sum(y*y for y in b))
docs=embed([r[0] for r in corpus]);queries=[q for row in corpus for q in row[1:]]
negative=['明天北京会下雨吗','晚饭吃什么菜','推荐一部科幻电影','月球到地球多远','李白写过哪些诗','今天股票大盘涨了多少','怎么做巧克力蛋糕','我妈妈的生日是什么时候','去上海旅游住哪里','帮我查一张机票']
report={}
for prefix in ('','为这个句子生成表示以用于检索相关文章：'):
 vectors=embed([prefix+q for q in queries+negative]);positive=[];ranks=[];bad=[]
 for n,v in enumerate(vectors):
  scores=[cosine(v,d) for d in docs]
  if n<len(queries):
   expected=n//5;positive.append(scores[expected]);ranks.append(sorted(range(len(scores)),key=lambda i:-scores[i]).index(expected)+1)
  else:bad.append(max(scores))
 report['instruction' if prefix else 'raw']={'positive_min':min(positive),'positive_p05':sorted(positive)[5],'negative_max':max(bad),'rank_at_5':sum(r<=5 for r in ranks)/len(ranks),'thresholds':{str(k):sum(s>=k and r<=5 for s,r in zip(positive,ranks))/len(ranks) for k in (.4,.45,.5,.55,.6,.65,.7,.72)}}
(root/'calibration.json').write_text(json.dumps(report,indent=2));print(json.dumps(report))
