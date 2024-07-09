package main

import (
	"derohe-proxy/config"
	"derohe-proxy/proxy"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deroproject/derohe/globals"
	"github.com/docopt/docopt-go"
)

// Initialize the logger
var logger *log.Logger

type WalletStats struct {
	Hashrate string `json:"hashrate"`
	Shares   uint64 `json:"shares"`
}

type Stats struct {
	Wallets map[string]WalletStats `json:"wallets"`
}

var walletStats = make(map[string]WalletStats)
var walletStatsMutex sync.Mutex

func init() {
	// Create a log file
	logFile, err := os.OpenFile("proxy.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Println("Failed to open log file:", err)
		os.Exit(1)
	}

	// Create a multi-writer that writes to both the log file and the console
	multiWriter := io.MultiWriter(logFile, os.Stdout)

	// Create a logger that writes to the multi-writer
	logger = log.New(multiWriter, "", log.LstdFlags|log.Lshortfile)
}

func main() {
	var err error

	config.Arguments, err = docopt.Parse(config.Command_line, nil, true, "pre-alpha", false)

	if err != nil {
		logger.Println("Error parsing arguments:", err)
		return
	}

	if config.Arguments["--listen-address"] != nil {
		addr, err := net.ResolveTCPAddr("tcp", config.Arguments["--listen-address"].(string))
		if err != nil {
			logger.Println("Error resolving listen address:", err)
			return
		} else {
			if addr.Port == 0 {
				logger.Println("Port not specified in listen address")
				return
			} else {
				config.Listen_addr = addr.String()
			}
		}
	}

	if config.Arguments["--daemon-address"] == nil {
		logger.Println("Daemon address not specified")
		return
	} else {
		config.Daemon_address = config.Arguments["--daemon-address"].(string)
	}

	if config.Arguments["--wallet-address"] != nil {

		// check for worker suffix
		var parseWorker []string
		var address string

		if strings.Contains(config.Arguments["--wallet-address"].(string), ".") {
			parseWorker = strings.Split(config.Arguments["--wallet-address"].(string), ".")
			config.Worker = parseWorker[1]
			address = parseWorker[0]
		} else {
			address = config.Arguments["--wallet-address"].(string)
		}

		addr, err := globals.ParseValidateAddress(address)
		if err != nil {
			logger.Printf("%v Wallet address is invalid!\n", time.Now().Format(time.Stamp))
		}
		config.WalletAddr = addr.String()
		if config.Worker != "" {
			logger.Printf("%v Using wallet %s and name %s for all connections\n", time.Now().Format(time.Stamp), config.WalletAddr, config.Worker)
		} else {
			logger.Printf("%v Using wallet %s for all connections\n", time.Now().Format(time.Stamp), config.WalletAddr)
		}
	}

	if config.Arguments["--log-interval"] != nil {
		interval, err := strconv.ParseInt(config.Arguments["--log-interval"].(string), 10, 32)
		if err != nil {
			logger.Println("Error parsing log interval:", err)
			return
		} else {
			if interval < 60 || interval > 3600 {
				config.Log_intervall = 60
			} else {
				config.Log_intervall = int(interval)
			}
		}
	}

	if config.Arguments["--nonce"].(bool) {
		config.Nonce = true
		logger.Printf("%v Nonce editing is enabled\n", time.Now().Format(time.Stamp))
	}

	if config.Arguments["--pool"].(bool) {
		config.Pool_mode = true
		config.Minimal = false
		logger.Printf("%v Pool mode is enabled\n", time.Now().Format(time.Stamp))
	}

	logger.Printf("%v Logging every %d seconds\n", time.Now().Format(time.Stamp), config.Log_intervall)

	go proxy.Start_server()

	// Wait for first miner connection to grab wallet address
	for proxy.CountMiners() < 1 {
		time.Sleep(time.Second * 1)
	}
	if config.Worker == "" {
		go proxy.Start_client(proxy.Address)
	} else {
		go proxy.Start_client(proxy.Address + "." + config.Worker)
	}

	for {
		time.Sleep(time.Second * time.Duration(config.Log_intervall))

		// Update statistics for each user session
		proxy.ClientListMutex.Lock()
		for _, session := range proxy.ClientList {
			address := session.Address.String()
			hash_rate_string := fmt.Sprintf("%v H/s", session.Hashrate) // Example: Format hashrate per session
			walletStatsMutex.Lock()
			walletStats[address] = WalletStats{
				Hashrate: hash_rate_string,
				Shares:   session.Shares, // Use individual shares
			}
			walletStatsMutex.Unlock()
		}
		proxy.ClientListMutex.Unlock()

		stats := Stats{Wallets: walletStats}

		statsFile, err := os.Create("stats.json")
		if err != nil {
			logger.Printf("Error creating stats file: %v\n", err)
			continue
		}

		encoder := json.NewEncoder(statsFile)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(stats)
		if err != nil {
			logger.Printf("Error writing stats to file: %v\n", err)
		} else {
			logger.Println("Successfully wrote stats to stats.json")
		}
		statsFile.Close()

		logWalletStats()
	}
}

func logWalletStats() {
	logger.Println("Connected Clients Stats:")
	walletStatsMutex.Lock()
	defer walletStatsMutex.Unlock()

	for address, stat := range walletStats {
		logger.Printf("Wallet: %s, Hashrate: %s, Shares: %d\n", address, stat.Hashrate, stat.Shares)
	}
}
