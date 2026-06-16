# docs/

Shareable assets — these are reproducible from the source:

- `demo.cast` — asciinema recording of `examples/demo/killer-demo.sh`
- `demo.gif`  — same cast rendered to GIF (embedded at the top of the main README)

To re-record both, run:

```sh
bash scripts/make-demo-gif.sh
```

The script does everything inside Docker (installs asciinema + downloads agg,
builds agentenv, runs the demo as **uid 1001 with no `--privileged`** to mirror a
restricted Kubernetes pod, then renders the cast to a GIF).
