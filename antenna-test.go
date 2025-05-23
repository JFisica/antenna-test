package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

type Config struct {
	ServerIP    string
	ServerPort  string
	Protocol    string  // "udp" or "tcp"
	PacketSize  int     // bytes
	DurationSec int     // test duration
	RateMbps    float64 // rate for UDP
}

type Result struct {
	Seq       int
	Timestamp time.Time
	RTT       time.Duration
	Lost      bool
	Duplicate bool
}

const timeMarshalSize = 15

func ParseFlags() Config {
	var cfg Config
	flag.StringVar(&cfg.ServerIP, "server", "127.0.0.1", "Server IP (0.0.0.0 to run as server)")
	flag.StringVar(&cfg.ServerPort, "port", "9000", "Server port")
	flag.StringVar(&cfg.Protocol, "proto", "udp", "Protocol: udp or tcp")
	flag.IntVar(&cfg.PacketSize, "size", 100, "Packet size in bytes")
	flag.IntVar(&cfg.DurationSec, "time", 5, "Test duration seconds")
	flag.Float64Var(&cfg.RateMbps, "rate", 1.0, "Rate Mbps (only UDP)")
	flag.Parse()
	return cfg
}

func main() {
	cfg := ParseFlags()
	if cfg.ServerIP == "0.0.0.0" || cfg.ServerIP == "localhost" {
		runServer(cfg)
	} else {
		runClient(cfg)
	}
}

func runServer(cfg Config) {
	addr := net.JoinHostPort(cfg.ServerIP, cfg.ServerPort)
	if cfg.Protocol == "udp" {
		runUDPServer(addr)
	} else {
		runTCPServer(addr)
	}
}

func runUDPServer(addr string) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		fmt.Println("Error resolving UDP addr:", err)
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		fmt.Println("Error listening UDP:", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 65535)
	fmt.Println("UDP server listening on", addr)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Read error:", err)
			continue
		}
		_, err = conn.WriteToUDP(buf[:n], clientAddr)
		if err != nil {
			fmt.Println("Write error:", err)
		}
	}
}

func runTCPServer(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("Error listening TCP:", err)
		return
	}
	defer ln.Close()
	fmt.Println("TCP server listening on", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			fmt.Println("Accept error:", err)
			continue
		}
		go handleTCPConnection(conn)
	}
}

func handleTCPConnection(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, err = conn.Write(buf[:n])
		if err != nil {
			return
		}
	}
}

func runClient(cfg Config) {
	addr := net.JoinHostPort(cfg.ServerIP, cfg.ServerPort)
	var results []Result
	if cfg.Protocol == "udp" {
		results = runUDPClient(addr, cfg)
	} else {
		results = runTCPClient(addr, cfg)
	}
	saveResultsCSV(results)
}

func runUDPClient(addr string, cfg Config) []Result {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		fmt.Println("Error resolving UDP addr:", err)
		return nil
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		fmt.Println("Error dialing UDP:", err)
		return nil
	}
	defer conn.Close()

	packetInterval := time.Duration(float64(cfg.PacketSize) / (cfg.RateMbps * 1e6) * 1e9)
	resultsCh := make(chan Result)
	stopSending := make(chan struct{})
	var wg sync.WaitGroup

	// Datos para reporte
	var results []Result
	receivedSeq := make(map[int]bool)
	var rtts []float64
	var totalBytesReceived int64
	seqCounter := 0

	collectorDone := make(chan struct{})
	go func() {
		for r := range resultsCh {
			// Imprimir resultado en tiempo real
			if r.Lost {
				fmt.Printf("Packet lost seq %d at %s\n", r.Seq, r.Timestamp.Format(time.RFC3339Nano))
			} else {
				rttMs := float64(r.RTT.Microseconds()) / 1000
				fmt.Printf("Packet seq %d received, RTT: %.3f ms\n", r.Seq, rttMs)
				// Para calcular jitter y throughput
				if !receivedSeq[r.Seq] {
					rtts = append(rtts, rttMs)
					totalBytesReceived += int64(cfg.PacketSize)
					receivedSeq[r.Seq] = true
				}
			}
			results = append(results, r)
		}
		close(collectorDone)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-stopSending:
					return
				default:
					continue
				}
			}
			if n < 4+timeMarshalSize {
				continue
			}
			seq := int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])
			var sentTime time.Time
			err = sentTime.UnmarshalBinary(buf[4 : 4+timeMarshalSize])
			if err != nil {
				continue
			}
			rtt := time.Since(sentTime)
			resultsCh <- Result{Seq: seq, Timestamp: sentTime, RTT: rtt, Lost: false}
		}
	}()

	deadline := time.Now().Add(time.Duration(cfg.DurationSec) * time.Second)
	sendBuf := make([]byte, cfg.PacketSize)

	for time.Now().Before(deadline) {
		seqCounter++
		ts := time.Now()
		sendBuf[0], sendBuf[1], sendBuf[2], sendBuf[3] = byte(seqCounter>>24), byte(seqCounter>>16), byte(seqCounter>>8), byte(seqCounter)
		tstampBytes, _ := ts.MarshalBinary()
		copy(sendBuf[4:], tstampBytes)

		_, err = conn.Write(sendBuf)
		if err != nil {
			fmt.Printf("Write error at seq %d: %v\n", seqCounter, err)
			resultsCh <- Result{Seq: seqCounter, Timestamp: ts, Lost: true}
		}

		time.Sleep(packetInterval)
	}

	time.Sleep(1 * time.Second)
	close(stopSending)
	wg.Wait()
	close(resultsCh)
	<-collectorDone

	// --- Reporte final ---
	jitter := calculateJitter(rtts)
	throughputMbps := float64(totalBytesReceived*8) / float64(cfg.DurationSec*1e6)
	latency := calculateLatencyStats(rtts)

	// Filtrar duplicados en el reporte final
	seen := make(map[int]bool)
	finalResults := []Result{}
	for _, r := range results {
		if !seen[r.Seq] || r.Duplicate {
			finalResults = append(finalResults, r)
			seen[r.Seq] = true
		}
	}

	fmt.Println("\n--- Test finalizado ---")
	fmt.Printf("Packets sent: %d\n", seqCounter)
	fmt.Printf("Packets received: %d\n", len(rtts))
	fmt.Printf("Packets lost: %d\n", seqCounter-len(rtts))
	fmt.Printf("Duplicates: %d\n", len(finalResults)-len(rtts))
	fmt.Printf("Average jitter: %.3f ms\n", jitter)
	fmt.Printf("Throughput: %.3f Mbps\n", throughputMbps)
	fmt.Printf("Latency min: %.3f ms\n", latency.Min)
	fmt.Printf("Latency max: %.3f ms\n", latency.Max)
	fmt.Printf("Latency avg: %.3f ms\n", latency.Mean)
	fmt.Printf("Latency stdev: %.3f ms\n", latency.Stdev)

	return finalResults
}

func calculateJitter(rtts []float64) float64 {
	if len(rtts) < 2 {
		return 0
	}
	var sumDiff float64
	for i := 1; i < len(rtts); i++ {
		diff := rtts[i] - rtts[i-1]
		if diff < 0 {
			diff = -diff
		}
		sumDiff += diff
	}
	return sumDiff / float64(len(rtts)-1)
}

type LatencyStats struct {
	Min   float64
	Max   float64
	Mean  float64
	Stdev float64
}

func calculateLatencyStats(rtts []float64) LatencyStats {
	if len(rtts) == 0 {
		return LatencyStats{}
	}

	min := rtts[0]
	max := rtts[0]
	sum := 0.0

	for _, rtt := range rtts {
		sum += rtt
		if rtt < min {
			min = rtt
		}
		if rtt > max {
			max = rtt
		}
	}

	mean := sum / float64(len(rtts))

	var variance float64
	for _, rtt := range rtts {
		diff := rtt - mean
		variance += diff * diff
	}
	stdev := math.Sqrt(variance / float64(len(rtts)))

	return LatencyStats{Min: min, Max: max, Mean: mean, Stdev: stdev}
}

func runTCPClient(addr string, cfg Config) []Result {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Println("Error dialing TCP:", err)
		return nil
	}
	defer conn.Close()

	buf := make([]byte, cfg.PacketSize)
	results := []Result{}
	seq := 0
	deadline := time.Now().Add(time.Duration(cfg.DurationSec) * time.Second)

	fmt.Printf("TCP client sending to %s\n", addr)

	for time.Now().Before(deadline) {
		seq++
		ts := time.Now()
		buf[0], buf[1], buf[2], buf[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		tstampBytes, err := ts.MarshalBinary()
		if err != nil {
			fmt.Println("Timestamp marshal error:", err)
			return nil
		}
		if len(tstampBytes)+4 > len(buf) {
			fmt.Println("Packet size too small for timestamp")
			return nil
		}
		copy(buf[4:], tstampBytes)

		_, err = conn.Write(buf)
		if err != nil {
			results = append(results, Result{Seq: seq, Timestamp: ts, Lost: true})
			continue
		}

		n, err := conn.Read(buf)
		if err != nil || n < 4 {
			results = append(results, Result{Seq: seq, Timestamp: ts, Lost: true})
			continue
		}

		var sentTime time.Time
		err = sentTime.UnmarshalBinary(buf[4:n])
		if err != nil {
			results = append(results, Result{Seq: seq, Timestamp: ts, Lost: true})
			continue
		}
		rtt := time.Since(sentTime)
		results = append(results, Result{Seq: seq, Timestamp: ts, RTT: rtt, Lost: false})
	}
	return results
}

func saveResultsCSV(results []Result) {
	file, err := os.Create("results.csv")
	if err != nil {
		fmt.Println("Error creating CSV:", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	writer.Write([]string{"Seq", "Timestamp", "RTT_ms", "Lost", "Duplicate"})
	for _, r := range results {
		rttMs := fmt.Sprintf("%.3f", float64(r.RTT.Microseconds())/1000)
		lostStr := strconv.FormatBool(r.Lost)
		dupStr := strconv.FormatBool(r.Duplicate)
		writer.Write([]string{
			strconv.Itoa(r.Seq),
			r.Timestamp.Format(time.RFC3339Nano),
			rttMs,
			lostStr,
			dupStr,
		})
	}
	fmt.Println("Resultados guardados en results.csv")
}
