# Public-endpoint recordings

Recorded by `.github/workflows/case-study.yml` on GitHub-hosted runners (Ubuntu x86_64 and macOS arm64) with the released Inspect 0.1.1 binary, checked against SHA256SUMS before use. Each target in `targets.txt` got 10 fresh-connection samples, one second apart, on October 21, 2026.

`results/{runner}/` holds the raw JSON recordings, the text explanations, two comparisons and the runner environment. The numbers in the bitspeed.org case study come from these files. Public endpoints change, so a new run will give different numbers.

To record again, edit `targets.txt` and push to main.
