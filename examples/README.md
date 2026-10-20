# Controlled Inspect demo

Run `python3 examples/demo.py /path/to/bitspeed-inspect /tmp/bitspeed-demo` from the repository. Python uses only its standard library. The demo starts a loopback fixture with a 200 ms header delay, records three requests, restarts the same address with a 20 ms delay and records three more. It saves before.json, after.json, explanation.txt and comparison.txt. No internet endpoint is used. The fixture processes are stopped automatically.

The saved recordings under recordings/ were generated with the downloaded first public Inspect binary. They are examples of the recording format and evidence trail, not a network benchmark or a promise of exact timings. Inspect can describe the observed first-byte interval change; client timings alone do not establish a server-side cause. Generate fresh files to explore measurements on your own machine.

Run `bitspeed-inspect explain examples/recordings/before.json` or `bitspeed-inspect compare examples/recordings/before.json examples/recordings/after.json` to inspect the saved evidence. Machine-specific timestamps, loopback port numbers and timings are intentionally preserved.
