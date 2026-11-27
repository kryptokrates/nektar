package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kryptokrates/nektar/internal/backup"
	"github.com/kryptokrates/nektar/internal/chain"
)

func cmdBackup(args []string) error {
	if len(args) == 0 {
		printBackupUsage()
		return nil
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	subArgs := args[1:]
	switch sub {
	case "now":
		return cmdBackupNow(subArgs)
	case "verify":
		return cmdBackupVerify(subArgs)
	case "restore":
		return cmdBackupRestore(subArgs)
	case "status":
		return cmdBackupStatus(subArgs)
	default:
		printBackupUsage()
		return fmt.Errorf("unknown backup subcommand %q", sub)
	}
}

func printBackupUsage() {
	fmt.Println(`nektar backup — BRC-42 envelope-encrypted, WAL-safe state backups

Usage:
  nektar backup now     [--out <dir>] [--keep 14] [--network testnet|main] [--data-dir <dir>]
  nektar backup verify  --file <archive.nktbak> [--network testnet|main] [--data-dir <dir>]
  nektar backup restore --file <archive.nktbak> --data-dir <target-dir> [--network testnet|main]
  nektar backup status  [--network testnet|main] [--data-dir <dir>]

Environment variables (automated background backups in nektar serve):
  NEKTAR_BACKUP_DIR          Local directory for atomic .nktbak snapshots
  NEKTAR_BACKUP_S3_BUCKET    S3 / Cloudflare R2 / Backblaze B2 / MinIO bucket (stdlib SigV4)
  NEKTAR_BACKUP_S3_ENDPOINT  Custom S3-compatible endpoint URL
  NEKTAR_BACKUP_S3_REGION    S3 region (default us-east-1)
  NEKTAR_BACKUP_S3_PREFIX    Optional object key prefix
  NEKTAR_BACKUP_S3_ACCESS_KEY / NEKTAR_BACKUP_S3_SECRET_KEY
  NEKTAR_BACKUP_INTERVAL     Schedule interval (default 6h)
  NEKTAR_BACKUP_KEEP         Retention count (default 14)`)
}

func cmdBackupNow(args []string) error {
	fs := flag.NewFlagSet("backup now", flag.ContinueOnError)
	outDir := fs.String("out", os.Getenv("NEKTAR_BACKUP_DIR"), "Local output directory (overrides NEKTAR_BACKUP_DIR / S3)")
	keep := fs.Int("keep", backup.ParseKeepEnv(os.Getenv("NEKTAR_BACKUP_KEEP")), "Number of archives to retain")
	netFlag := fs.String("network", "", "Network override")
	dataDirFlag := fs.String("data-dir", "", "Data directory override")
	confirmFlag := fs.String("confirm", "", "Mainnet confirmation (I_UNDERSTAND)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netFlag, *dataDirFlag, "")
	if err != nil {
		return err
	}
	defer rt.closeFn()

	var sink backup.Sink
	if strings.TrimSpace(*outDir) != "" {
		sink, err = backup.NewLocalDirSink(*outDir)
	} else {
		sink, err = backup.NewSinkFromEnv(rt.mem.Chain().Network())
	}
	if err != nil {
		return err
	}
	if sink == nil {
		defaultDir := filepath.Join(rt.dataDir, "backups")
		sink, err = backup.NewLocalDirSink(defaultDir)
		if err != nil {
			return err
		}
	}

	mgr := backup.NewManager(backup.Config{
		DB:      rt.mem.DB(),
		Port:    rt.mem.Chain(),
		DataDir: rt.dataDir,
		Sink:    sink,
		Keep:    *keep,
	})
	res, err := mgr.BackupNow(ctx)
	if err != nil {
		return err
	}
	enc, _ := json.MarshalIndent(map[string]any{
		"ok":     true,
		"result": res,
		"status": mgr.Status(),
	}, "", "  ")
	fmt.Println(string(enc))
	return nil
}

func cmdBackupVerify(args []string) error {
	fs := flag.NewFlagSet("backup verify", flag.ContinueOnError)
	fileFlag := fs.String("file", "", "Path to .nktbak encrypted archive")
	netFlag := fs.String("network", "", "Network override")
	dataDirFlag := fs.String("data-dir", "", "Data directory holding wallet/identity.json (or BSV Desktop mode)")
	confirmFlag := fs.String("confirm", "", "Mainnet confirmation (I_UNDERSTAND)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targetFile := strings.TrimSpace(*fileFlag)
	if targetFile == "" && fs.NArg() > 0 {
		targetFile = strings.TrimSpace(fs.Arg(0))
	}
	if targetFile == "" {
		return errors.New("backup verify requires --file <archive.nktbak>")
	}
	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}

	archiveBytes, err := os.ReadFile(targetFile)
	if err != nil {
		return err
	}
	ctx := context.Background()
	netName := *netFlag
	if netName == "" {
		netName = envOr("NEKTAR_NETWORK", "testnet")
	}
	dir := *dataDirFlag
	if dir == "" {
		dir = defaultDataDirForNetwork(netName)
	}
	port, err := chain.Open(ctx, chain.Config{
		Network:        netName,
		DataDir:        dir,
		WalletMode:     envOr("NEKTAR_WALLET", "auto"),
		WalletWireURL:  envOr("NEKTAR_WALLET_WIRE_URL", "http://127.0.0.1:3321"),
		MainnetConfirm: os.Getenv("NEKTAR_MAINNET_CONFIRM"),
	})
	if err != nil {
		return err
	}
	defer port.Close()

	manifest, err := backup.VerifyArchive(ctx, port.WalletInterface(), archiveBytes)
	if err != nil {
		return err
	}
	enc, _ := json.MarshalIndent(map[string]any{
		"ok":       true,
		"file":     targetFile,
		"manifest": manifest,
	}, "", "  ")
	fmt.Println(string(enc))
	return nil
}

func cmdBackupRestore(args []string) error {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	fileFlag := fs.String("file", "", "Path to .nktbak encrypted archive")
	netFlag := fs.String("network", "", "Network override")
	dataDirFlag := fs.String("data-dir", "", "Target data directory to restore into (must not be running)")
	walletDirFlag := fs.String("wallet-dir", "", "Optional directory holding wallet/identity.json if different from --data-dir")
	confirmFlag := fs.String("confirm", "", "Mainnet confirmation (I_UNDERSTAND)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targetFile := strings.TrimSpace(*fileFlag)
	if targetFile == "" && fs.NArg() > 0 {
		targetFile = strings.TrimSpace(fs.Arg(0))
	}
	if targetFile == "" {
		return errors.New("backup restore requires --file <archive.nktbak>")
	}
	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}
	netName := *netFlag
	if netName == "" {
		netName = envOr("NEKTAR_NETWORK", "testnet")
	}
	targetDir := strings.TrimSpace(*dataDirFlag)
	if targetDir == "" {
		targetDir = defaultDataDirForNetwork(netName)
	}
	// Fail closed if an active Nektar daemon holds instance.lock on targetDir!
	releaseLock, lockErr := acquireInstanceLock(targetDir, "backup-restore")
	if lockErr != nil {
		return fmt.Errorf("cannot restore into active running data-dir (%s): stop nektar first: %w", targetDir, lockErr)
	}
	defer releaseLock()

	archiveBytes, err := os.ReadFile(targetFile)
	if err != nil {
		return err
	}
	ctx := context.Background()
	wDir := strings.TrimSpace(*walletDirFlag)
	if wDir == "" {
		wDir = targetDir
	}
	port, err := chain.Open(ctx, chain.Config{
		Network:        netName,
		DataDir:        wDir,
		WalletMode:     envOr("NEKTAR_WALLET", "auto"),
		WalletWireURL:  envOr("NEKTAR_WALLET_WIRE_URL", "http://127.0.0.1:3321"),
		MainnetConfirm: os.Getenv("NEKTAR_MAINNET_CONFIRM"),
	})
	if err != nil {
		return err
	}
	// Close port BEFORE overwriting targetDir/wallet/wallet.sqlite!
	// Wait: we need WalletInterface() to decrypt first; VerifyArchive runs on a copy, and we close port only if wDir == targetDir right after decrypt?
	// Even cleaner: restore into a staging subdir inside targetDir first? Actually RestoreArchive writes wallet.sqlite only AFTER decrypting,
	// so if wDir == targetDir on Windows, open SQLite handle on wallet.sqlite could block rename.
	// Therefore, stage into temp dir while port is open, close port, then move restored files into targetDir!
	stageDir, err := os.MkdirTemp("", "nektar-restore-stage-*")
	if err != nil {
		_ = port.Close()
		return err
	}
	defer os.RemoveAll(stageDir)

	manifest, err := backup.RestoreArchive(ctx, port.WalletInterface(), archiveBytes, stageDir)
	_ = port.Close()
	if err != nil {
		return err
	}
	for _, rel := range []string{"nektar.sqlite", "wallet/wallet.sqlite", "spend-guard.json"} {
		src := filepath.Join(stageDir, filepath.FromSlash(rel))
		data, rErr := os.ReadFile(src)
		if rErr != nil {
			continue
		}
		dst := filepath.Join(targetDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".sqlite") {
			_ = os.Remove(dst + "-wal")
			_ = os.Remove(dst + "-shm")
		}
	}

	enc, _ := json.MarshalIndent(map[string]any{
		"ok":        true,
		"restored":  targetDir,
		"manifest":  manifest,
		"nextSteps": "Start nektar serve/up normally to verify restored state.",
	}, "", "  ")
	fmt.Println(string(enc))
	return nil
}

func cmdBackupStatus(args []string) error {
	fs := flag.NewFlagSet("backup status", flag.ContinueOnError)
	netFlag := fs.String("network", "", "Network override")
	dataDirFlag := fs.String("data-dir", "", "Data directory override")
	confirmFlag := fs.String("confirm", "", "Mainnet confirmation (I_UNDERSTAND)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*confirmFlag) != "" {
		_ = os.Setenv("NEKTAR_MAINNET_CONFIRM", strings.TrimSpace(*confirmFlag))
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netFlag, *dataDirFlag, "")
	if err != nil {
		return err
	}
	defer rt.closeFn()

	sink, sinkErr := backup.NewSinkFromEnv(rt.mem.Chain().Network())
	mgr := backup.NewManager(backup.Config{
		DB:       rt.mem.DB(),
		Port:     rt.mem.Chain(),
		DataDir:  rt.dataDir,
		Sink:     sink,
		Interval: backup.ParseIntervalEnv(os.Getenv("NEKTAR_BACKUP_INTERVAL")),
		Keep:     backup.ParseKeepEnv(os.Getenv("NEKTAR_BACKUP_KEEP")),
	})
	out := map[string]any{
		"ok":     sinkErr == nil,
		"status": mgr.Status(),
	}
	if sinkErr != nil {
		out["sinkError"] = sinkErr.Error()
	}
	if sink != nil {
		if list, lErr := sink.List(ctx); lErr == nil {
			out["archives"] = list
		}
	}
	enc, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(enc))
	return nil
}
