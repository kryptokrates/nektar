# nektar bstore deployment

A full bstore stack on one host: **host** (directory + `ls_bstore`) +
**provider** (bounty-paid piece storage) + **S3 gateway** (`s3d`) + MySQL.
Testnet by default — `BSTORE_NETWORK=main` flips every chain reference in
`.env` (the comments show the mainnet values).

## Bring it up

```bash
cp .env.example .env        # pick network, set BSTORE_S3_SECRET
docker compose --profile setup up certs   # provider's internal CA (one-off)
docker compose up -d                       # db + host + provider + gateway
```

### Identities + funding (testnet)

Each service keeps its own wallet — init it, print the fund address, drip
from the testnet faucet, import:

```bash
# provider identity
docker compose run --rm provider init -data /var/lib/bstore-provider
docker compose run --rm provider address -config /etc/bstore/provider.json
docker compose run --rm provider fund -config /etc/bstore/provider.json -txid <TXID>

# gateway's `gw` identity — same three steps with s3d subcommand
docker compose run --rm gateway s3d init -config /etc/bstore/gateway.json -identity gw
docker compose run --rm gateway s3d address -config /etc/bstore/gateway.json -identity gw
docker compose run --rm gateway s3d fund -config /etc/bstore/gateway.json -identity gw -txid <TXID>
```

Then point the instance's object store at the gateway (SigV4, path-style):

```bash
NEKTAR_BSTORE_S3_URL=http://127.0.0.1:9000
NEKTAR_BSTORE_BUCKET=<s3 bucket name — bstore bucket id with '-' for '_'>
NEKTAR_BSTORE_ACCESS_KEY=nektar          # matches access_keys[0].id in gateway config
NEKTAR_BSTORE_SECRET_KEY=<BSTORE_S3_SECRET from .env>
```

The gateway's `bounty` config sets the put bounty (satoshis/t/k/retention) — there is no per-request bounty header. Nektar verifies every read against its own SHA-256 ref; possession proofs and settlement stay with the providers.

## Verifying

```bash
# the gateway answers its health endpoint
curl -s http://127.0.0.1:9000/v1/health

# a put from the owner CLI writes through the whole stack
docker compose run --rm cli owner bstore put smoke /tmp/file.txt
```
