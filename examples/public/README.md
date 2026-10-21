# Public-endpoint recordings

Recorded by `.github/workflows/case-study.yml` on GitHub-hosted runners (Ubuntu x86_64 and macOS arm64) with the released Inspect 0.1.1 binary, checked against SHA256SUMS before use. Each target in `targets.txt` got 10 fresh-connection samples, one second apart, on October 21, 2026.

`results/{runner}/` holds the raw JSON recordings, the text explanations, two comparisons and the runner environment. The numbers in the bitspeed.org case study come from these files. Public endpoints change, so a new run will give different numbers.

To record again, edit `targets.txt` and push to main.

`warm/{runner}/` holds fresh and warm-connection recordings made by `.github/workflows/case-study-warm.yml` with Inspect 0.3.0, triggered by `warm-targets.txt`.

`windows-demo/github-windows/` is a Transfer delivery recorded by `.github/workflows/windows-demo.yml` on a GitHub Windows runner with the published Windows archive named in `windows-demo-release.txt`, checked against SHA256SUMS first. `scripts/demo-transfer-resume.py` prepares a 64 MiB file in 1 MiB chunks, slows each upload a little so the kill lands part way, kills sender and receiver, restarts the receiver, asks what it can prove, resumes, checks the delivered SHA-256 and runs clean. Each step's output is kept. To record again, change the release tag in `windows-demo-release.txt` and push to main.
