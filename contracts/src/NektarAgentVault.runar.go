// NektarAgentVault — two-key consensus-enforced agent spending vault.
// Separates autonomous hot-agent execution from cold-operator authority on BSV L1:
//   - AgentSpend: hot agent key may spend up to PerTxCap per tx and WindowCap per
//     rolling WindowBlocks window into ANY destination script (including directly
//     funding a DisclosureSale escrow in 1 atomic tx), strictly refusing when Paused == 1
//     or when nSequence == 0xffffffff / timestamp locktime >= 500_000_000 (F-02).
//   - OperatorControl: cold operator key may pause/unpause (consensus kill switch),
//     rotate AgentPkh, tune caps, or top up the vault balance.
//   - OperatorSweep: cold operator key may close the vault and sweep all remaining
//     satoshis to any operator-chosen P2PKH destination.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarAgentVault struct {
	runar.StatefulSmartContract
	OperatorPkh   runar.ByteString // 20B cold operator pubkey hash (immutable)
	AgentPkh      runar.ByteString // 20B hot agent pubkey hash (rotatable by operator)
	PerTxCap      runar.Bigint     // max sats per single agent spend
	WindowCap     runar.Bigint     // max sats per rolling window
	WindowBlocks  runar.Bigint     // rolling window length in blocks (e.g. 144)
	MaxFee        runar.Bigint     // max miner fee per agent spend tx
	SpentInWindow runar.Bigint     // cumulative sats spent in current window
	WindowStart   runar.Bigint     // block height where current window began
	Paused        runar.Bigint     // 0 = active, 1 = paused (consensus kill switch)
}

func (c *NektarAgentVault) Init() {
	c.SpentInWindow = 0
	c.WindowStart = 0
	c.Paused = 0
}

func (c *NektarAgentVault) p2pkh(pkh runar.ByteString) runar.ByteString {
	return runar.Cat(runar.HexToBytes("76a914"), runar.Cat(pkh, runar.HexToBytes("88ac")))
}

// AgentSpend authorizes an autonomous agent purchase (directly into a DisclosureSale escrow
// locking script or P2PKH destination) bounded by PerTxCap, WindowCap, MaxFee, and Paused == 0.
func (c *NektarAgentVault) AgentSpend(
	sig runar.Sig,
	pubKey runar.PubKey,
	destScript runar.ByteString,
	payAmount runar.Bigint,
	newSpent runar.Bigint,
	newStart runar.Bigint,
	contSats runar.Bigint,
) {
	runar.Assert(c.Paused == 0)
	runar.Assert(runar.Len(c.OperatorPkh) == 20 && runar.Len(c.AgentPkh) == 20)
	runar.Assert(runar.Hash160(pubKey) == c.AgentPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.WindowStart)
	runar.Assert(payAmount >= 1 && contSats >= 1)
	runar.Assert(runar.Len(destScript) >= 1)
	expired := lt >= c.WindowStart+c.WindowBlocks
	runar.Assert((expired && newSpent == payAmount && newStart == lt) ||
		(!expired && newSpent == c.SpentInWindow+payAmount && newStart == c.WindowStart))
	runar.Assert(newSpent <= c.WindowCap)
	runar.Assert(payAmount <= c.PerTxCap)
	inSats := runar.ExtractAmount(c.TxPreimage)
	runar.Assert(inSats >= contSats+payAmount)
	runar.Assert(inSats-contSats-payAmount <= c.MaxFee)
	c.SpentInWindow = newSpent
	c.WindowStart = newStart
	c.AddOutput(contSats, c.OperatorPkh, c.AgentPkh, c.PerTxCap, c.WindowCap, c.WindowBlocks, c.MaxFee, c.SpentInWindow, c.WindowStart, c.Paused)
	c.AddRawOutput(payAmount, destScript)
}

// OperatorControl lets the cold operator pause/resume, rotate the hot agent key,
// update spending limits, or top up the vault balance without moving custody.
func (c *NektarAgentVault) OperatorControl(
	sig runar.Sig,
	pubKey runar.PubKey,
	newAgentPkh runar.ByteString,
	newPerTxCap runar.Bigint,
	newWindowCap runar.Bigint,
	newWindowBlocks runar.Bigint,
	newMaxFee runar.Bigint,
	newPaused runar.Bigint,
	contSats runar.Bigint,
) {
	runar.Assert(runar.Len(c.OperatorPkh) == 20)
	runar.Assert(runar.Hash160(pubKey) == c.OperatorPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(runar.Len(newAgentPkh) == 20)
	runar.Assert(newPerTxCap >= 1 && newWindowCap >= newPerTxCap && newWindowBlocks >= 1 && newMaxFee >= 1)
	runar.Assert(newPaused == 0 || newPaused == 1)
	runar.Assert(contSats >= 1)
	c.AgentPkh = newAgentPkh
	c.PerTxCap = newPerTxCap
	c.WindowCap = newWindowCap
	c.WindowBlocks = newWindowBlocks
	c.MaxFee = newMaxFee
	c.Paused = newPaused
	c.AddOutput(contSats, c.OperatorPkh, c.AgentPkh, c.PerTxCap, c.WindowCap, c.WindowBlocks, c.MaxFee, c.SpentInWindow, c.WindowStart, c.Paused)
}

// OperatorSweep lets the cold operator terminate the vault and sweep all remaining funds to destPkh.
func (c *NektarAgentVault) OperatorSweep(
	sig runar.Sig,
	pubKey runar.PubKey,
	destPkh runar.ByteString,
	sweepSats runar.Bigint,
) {
	runar.Assert(runar.Len(c.OperatorPkh) == 20)
	runar.Assert(runar.Hash160(pubKey) == c.OperatorPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(runar.Len(destPkh) == 20)
	runar.Assert(sweepSats >= 1)
	c.AddRawOutput(sweepSats, c.p2pkh(destPkh))
}
