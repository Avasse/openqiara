# domusvm testdata

This directory holds the bytecode files (`<hash>.bin`) the unit tests
use, and `update_manifest.json`, an extract of the camera's
`/etc/hl/update_manifest.json`. Bytecode files are proprietary Qiara
firmware and are **not** committed to this repository — fetch them
yourself from a camera you own:

```bash
ssh -i ~/.ssh/<your-key> root@<cam-ip> \
    "cat /lib/firmwares/bytecode/<hash>.bin" \
    > internal/domusvm/testdata/<hash>.bin
```

Without these files the bytecode unit tests will skip with a "testdata
missing" message; the rest of the suite still runs.
