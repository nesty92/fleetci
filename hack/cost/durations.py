# Job durations from GHARuns' jobs.parquet: what GitHub bills (each job
# rounded up to a minute) against fleetci's VM time (per second, 60 s
# minimum, plus boot overhead). Usage: durations.py DATA_DIR
import sys, duckdb
G = sys.argv[1]
c = duckdb.connect(); c.execute("SET memory_limit='4GB'")
c.execute(f"""create table b as select epoch(cast(completed_at as timestamptz)) - epoch(cast(started_at as timestamptz)) d
  from '{G}/jobs.parquet' where status='COMPLETED' and conclusion in ('SUCCESS','FAILURE','CANCELLED','TIMED_OUT')""")
c.execute("delete from b where d is null or d<=0 or d>6*3600")
n, run, mean, q, gh, a45, a90 = c.execute("""select count(*), sum(d)/60, avg(d), quantile_cont(d,[0.5,0.9,0.99]),
  sum(ceil(d/60)), sum(greatest(60,d+45))/60, sum(greatest(60,d+90))/60 from b""").fetchone()
print(f"jobs {n:,}; mean {mean:.0f}s; p50/p90/p99 {[round(x) for x in q]}s")
print(f"per job: run {run/n:.2f} min, GitHub billed {gh/n:.2f} min, fleetci VM {a45/n:.2f}-{a90/n:.2f} min")
