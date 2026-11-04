# Joins every GHARuns job run to its runs-on tier, from the workflow files
# parse_runs_on.py read. Usage: join_sizes.py DIR_WITH_classify.py DATA_DIR
import json, sys, duckdb
sys.path.insert(0, sys.argv[1]); from classify import classify
G = sys.argv[2]
defs = []
for line in open(f"{G}/jobs_runs_on.jsonl"):
    o = json.loads(line)
    cls = classify(o["runs_on"])
    mv = []
    if o["matrix"]:
        for v in o["matrix"]:
            c = classify(v)
            if c: mv.append({"v": (v if isinstance(v, str) else " ".join(map(str, v)) if isinstance(v, list) else json.dumps(v)).lower(), "c": c})
        if cls is None and mv and len({m["c"] for m in mv}) == 1: cls = mv[0]["c"]
    defs.append({"h": o["h"], "key": o["key"].lower(), "name": (o["name"] or o["key"]).lower(), "cls": cls, "mv": mv if cls is None else []})
with open(f"{G}/jobdefs.jsonl", "w") as f:
    for d in defs: f.write(json.dumps(d) + "\n")
c = duckdb.connect(f"{G}/work.duckdb"); c.execute("SET threads=8"); c.execute("SET memory_limit='4GB'"); c.execute("SET temp_directory='/home/nesty/.cache/fleetci-research/spill'"); c.execute("SET preserve_insertion_order=false")
c.execute(f"create or replace table jobdefs as select * from read_json('{G}/jobdefs.jsonl', columns={{h:'VARCHAR',key:'VARCHAR',name:'VARCHAR',cls:'VARCHAR',mv:'STRUCT(v VARCHAR, c VARCHAR)[]'}})")
c.execute("create or replace table filecls as select h, any_value(cls) cls from jobdefs group by h having count(distinct cls)=1 and count(*)=count(cls)")
c.execute(f"""create table if not exists jr as
  select lower(j.name) name, epoch(cast(j.completed_at as timestamptz)) - epoch(cast(j.started_at as timestamptz)) d, w.file_hash h
  from '{G}/jobs.parquet' j join (select distinct check_suite_id, lower(repository) repo, path from '{G}/runs.parquet') r using (check_suite_id)
  left join want w on w.repo = r.repo and w.path = r.path
  where j.status='COMPLETED' and j.conclusion in ('SUCCESS','FAILURE','CANCELLED','TIMED_OUT')""")
c.execute("delete from jr where d is null or d<=0 or d>6*3600")
c.execute("""create or replace table sized as
  with m as (
    select jr.rowid rid, coalesce(jd.cls, list_filter(jd.mv, lambda x: strpos(jr.name, x.v) > 0)[1].c) c
    from jr join jobdefs jd
      on jd.h = jr.h and (regexp_replace(jr.name, ' \\(.*\\)$', '') in (jd.name, jd.key))
  ), pick as (select rid, any_value(c) c from m group by rid)
  select jr.d, coalesce(p.c, f.cls, case when jr.h is null then 'no-workflow-file' else 'unmatched' end) cls
  from jr left join pick p on p.rid = jr.rowid left join filecls f on f.h = jr.h""")
for r in c.execute("""select cls, count(*) n, round(100.0*count(*)/sum(count(*)) over(),1) pct_jobs,
   round(100.0*sum(d)/sum(sum(d)) over(),1) pct_min, median(d) med_s, round(avg(d)) mean_s,
   round(sum(ceil(d/60))/count(*),2) gh_billed_min_per_job
   from sized group by 1 order by 2 desc""").fetchall(): print(r)
