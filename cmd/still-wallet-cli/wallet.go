package main

import (
	"flag"
	"fmt"
	"os"
	"still-blockchain/address"
	"still-blockchain/config"
	"still-blockchain/logger"
	"still-blockchain/util"
	"still-blockchain/wallet"
	"strings"

	"github.com/ergochat/readline"
)

var Log = logger.New()

var default_rpc = fmt.Sprintf("http://127.0.0.1:%d", config.RPC_BIND_PORT)

func initialPrompt() *wallet.Wallet {
	l, err := readline.NewEx(&readline.Config{
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",

		HistorySearchFold: true,
	})
	if err != nil {
		panic(err)
	}
	defer l.Close()

	lcfg := l.GeneratePasswordConfig()
	lcfg.MaskRune = '*'

	for {
		Log.Info("Available commands:")
		Log.Info("open    Open wallet file")
		Log.Info("create  Creates a new wallet")
		Log.Info("restore Restore a wallet from seedphrase")

		l.SetPrompt("\033[32m>\033[0m ")

		line, err := l.ReadLine()
		if err != nil {
			Log.Err(err)
			os.Exit(0)
		}

		cmds := strings.Split(strings.ToLower(strings.ReplaceAll(line, "  ", " ")), " ")

		if len(cmds) == 0 {
			Log.Err("invalid command")
			continue
		}

		cmd := cmds[0]
		if cmd != "open" && cmd != "create" && cmd != "restore" {
			Log.Err("unknown command")
			continue
		}
		if len(cmds) == 1 {
			l.SetPrompt("Wallet name: ")
			filename, err := l.ReadLine()
			if err != nil {
				Log.Err(err)
				os.Exit(0)
			}
			l.SetPrompt("\033[32m>\033[0m ")

			if len(filename) == 0 {
				Log.Err("wallet name is too short")
				continue
			}

			cmds = append(cmds, filename)
		}

		fmt.Print("Wallet password: ")
		password, err := l.ReadLineWithConfig(lcfg)
		if err != nil {
			Log.Err(err)
		}

		if cmds[0] == "open" {
			Log.Info("opening wallet")

			w, err := wallet.OpenWalletFile(default_rpc, cmds[1]+".keys", []byte(password))
			if err != nil {
				Log.Err(err)
				continue
			}
			return w
		} else {
			fmt.Print("Repeat password: ")
			confirmPass, err := l.ReadLineWithConfig(lcfg)
			if err != nil {
				Log.Err(err)
				continue
			}
			if string(confirmPass) != string(password) {
				Log.Err("password doesn't match")
			}

			if cmd == "create" {
				w, err := wallet.CreateWalletFile(default_rpc, cmds[1]+".keys", []byte(password))
				if err != nil {
					Log.Err("Could not create wallet:", err)
					continue
				}

				return w
			} else if cmd == "restore" {
				Log.Info("restoring wallet")

				l.SetPrompt("Mnemonic seed: ")
				mnemonic, err := l.ReadLine()
				if err != nil {
					Log.Err(err)
					os.Exit(0)
				}

				w, err := wallet.CreateWalletFileFromMnemonic("http://127.0.0.1:6311", cmds[1]+".keys",
					mnemonic, []byte(password))
				if err != nil {
					Log.Err(err)
					continue
				}
				return w
			}
		}
	}
}

func main() {
	log_level := flag.Uint("log-level", 0, "sets the log level (range: 0-2)")
	rpc_bind_ip := flag.String("rpc-bind-ip", "127.0.0.1", "starts RPC server on this IP")
	rpc_bind_port := flag.Uint("rpc-bind-port", 0, "starts RPC server on this port")
	rpc_auth := flag.String("rpc-auth", "", "colon-separated username and password, like user:pass")
	open_wallet := flag.String("open-wallet", "", "open a wallet file")
	wallet_password := flag.String("wallet-password", "", "wallet password when using --open-wallet")

	flag.Parse()

	Log.SetLogLevel(uint8(*log_level))

	Log.Info("Starting STILL Wallet CLI")

	var w *wallet.Wallet

	if len(*open_wallet) > 0 {
		wallname := *open_wallet

		if strings.ContainsAny(wallname, "/. \\$") {
			Log.Fatal("invalid wallet name")
		}
		var err error
		w, err = wallet.OpenWalletFile(default_rpc, wallname+".keys", []byte(*wallet_password))
		if err != nil {
			Log.Fatal(err)
		}
	} else {
		w = initialPrompt()
	}

	if *rpc_bind_port != 0 {
		if len(*rpc_auth) < 7 {
			Log.Err("rpc-auth is invalid or too short")
		}
		Log.Infof("Starting wallet rpc server on %v:%v", *rpc_bind_ip, *rpc_bind_port)
		startRpcServer(w, *rpc_bind_ip, uint16(*rpc_bind_port), *rpc_auth)
	}

	addr := w.GetAddress()
	if addr.Addr == address.INVALID_ADDRESS {
		Log.Fatal("wallet has invalid address")
	}

	Log.Info("Wallet", w.GetAddress(), "has been loaded")

	Log.Debugf("Address hex: %x", addr.Addr[:])

	err := w.Refresh()
	if err != nil {
		Log.Warn("refresh failed:", err)
	} else {
		Log.Info("Balance:", util.FormatCoin(w.GetBalance()))
		Log.Info("Last nonce:", w.GetLastNonce())
	}

	prompts(w)
}
