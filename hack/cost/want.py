# For every workflow that ran jobs in GHARuns, the hash of its current file
# in the workflow-histories dataset. Writes the "want" table to
# DATA_DIR/work.duckdb and the hashes to want_hashes.txt. Usage: want.py DATA_DIR
import sys, duckdb
G = sys.argv[1]
c = duckdb.connect(f"{G}/work.duckdb"); c.execute("SET memory_limit='4GB'")
c.execute(f"""create or replace table wf_used as
  select lower(repository) repo, path, count(*) runs from '{G}/runs.parquet'
  where check_suite_id in (select distinct check_suite_id from '{G}/jobs.parquet') group by 1,2""")
c.execute(f"""create or replace table wf_latest as
  select lower(repository) repo, file_path path, arg_max(file_hash, committed_date) file_hash
  from read_csv('{G}/workflows.csv.gz', header=true, all_varchar=true)
  where git_change_type <> 'D' and valid_workflow='True' group by 1,2""")
c.execute("create or replace table want as select u.repo, u.path, u.runs, l.file_hash from wf_used u left join wf_latest l using (repo, path)")
print(c.execute("select count(*), count(file_hash), sum(runs), sum(case when file_hash is not null then runs end) from want").fetchone())
c.execute(f"copy (select distinct file_hash from want where file_hash is not null) to '{G}/want_hashes.txt' (header false)")
