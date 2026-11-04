# Cost comparison data

Reproduces [docs/cost.md](../../docs/cost.md) from the public GHARuns and
GitHub Actions workflow-histories datasets. Keep the data on a real disk:
the join spills several GB, and `/tmp` is often RAM.

```sh
d=~/.cache/fleetci-research; mkdir -p $d && cd $d
z=https://zenodo.org/api/records
curl -fLo jobs.parquet $z/20427454/files/jobs.parquet/content       # 1.6 GB
curl -fLo runs.parquet $z/20427454/files/runs.parquet/content       # 1.4 GB
curl -fLo workflows.csv.gz $z/20340547/files/workflows.csv.gz/content
pip install duckdb pyyaml

python durations.py $d                                              # durations and billed minutes
python want.py $d                                                   # each workflow's current file
curl -fL $z/20340547/files/workflows.tar.gz/content | python parse_runs_on.py $d/want_hashes.txt $d/jobs_runs_on.jsonl
python join_sizes.py . $d                                           # jobs by runner tier
```
