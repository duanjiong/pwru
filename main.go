// SPDX-License-Identifier: GPL-2.0-only
/* Copyright (C) 2020-2021 Martynas Pumputis */
/* Copyright (C) 2021 Authors of Cilium */

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	pb "github.com/cheggaaa/pb/v3"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"golang.org/x/sys/unix"

	"github.com/cilium/pwru/internal/pwru"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang KProbePWRU ./bpf/kprobe_pwru.c -- -DOUTPUT_SKB -I./bpf/headers
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang KProbePWRUWithoutOutputSKB ./bpf/kprobe_pwru.c -- -I./bpf/headers

type Foo interface {
	Close() error
}

func main() {
	var (
		kprobe1, kprobe2, kprobe3, kprobe4, kprobe5 *ebpf.Program
		cfgMap, events, printSkbMap                 *ebpf.Map
	)

	flags := pwru.Flags{
		FilterMark:       flag.Int("filter-mark", 0, "filter skb mark"),
		FilterProto:      flag.String("filter-proto", "", "filter L4 protocol (tcp, udp, icmp)"),
		FilterSrcIP:      flag.String("filter-src-ip", "", "filter source IP addr"),
		FilterDstIP:      flag.String("filter-dst-ip", "", "filter destination IP addr"),
		FilterSrcPort:    flag.String("filter-src-port", "", "filter source port"),
		FilterDstPort:    flag.String("filter-dst-port", "", "filter destination port"),
		OutputRelativeTS: flag.Bool("output-relative-timestamp", false, "print relative timestamp per skb"),
		OutputMeta:       flag.Bool("output-meta", false, "print skb metadata"),
		OutputTuple:      flag.Bool("output-tuple", false, "print L4 tuple"),
		OutputSkb:        flag.Bool("output-skb", false, "print skb"),
	}
	flag.Parse()

	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{
		Cur: 4096,
		Max: 4096,
	}); err != nil {
		log.Fatalf("failed to set temporary rlimit: %s", err)
	}
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{
		Cur: unix.RLIM_INFINITY,
		Max: unix.RLIM_INFINITY,
	}); err != nil {
		log.Fatalf("Failed to set temporary rlimit: %s", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		log.Println("Received signal, exiting program..")
	}()
	defer stop()

	funcs, err := pwru.GetFuncs()
	if err != nil {
		log.Fatalf("Failed to get skb-accepting functions: %s", err)
	}
	addr2name, err := pwru.GetAddrs(funcs)
	if err != nil {
		log.Fatalf("Failed to get function addrs: %s", err)
	}

	if *flags.OutputSkb {
		objs := KProbePWRUObjects{}
		if err := LoadKProbePWRUObjects(&objs, nil); err != nil {
			log.Fatalf("Loading objects: %v", err)
		}
		defer objs.Close()
		kprobe1 = objs.KprobeSkb1
		kprobe2 = objs.KprobeSkb2
		kprobe3 = objs.KprobeSkb3
		kprobe4 = objs.KprobeSkb4
		kprobe5 = objs.KprobeSkb5
		cfgMap = objs.CfgMap
		events = objs.Events
		printSkbMap = objs.PrintSkbMap
	} else {
		objs := KProbePWRUWithoutOutputSKBObjects{}
		if err := LoadKProbePWRUWithoutOutputSKBObjects(&objs, nil); err != nil {
			log.Fatalf("Loading objects: %v", err)
		}
		defer objs.Close()
		kprobe1 = objs.KprobeSkb1
		kprobe2 = objs.KprobeSkb2
		kprobe3 = objs.KprobeSkb3
		kprobe4 = objs.KprobeSkb4
		kprobe5 = objs.KprobeSkb5
		cfgMap = objs.CfgMap
		events = objs.Events
	}

	pwru.ConfigBPFMap(&flags, cfgMap)

	log.Println("Attaching kprobes...")
	ignored := 0
	bar := pb.StartNew(len(funcs))
	for name, pos := range funcs {
		fn := kprobe1
		switch pos {
		case 1:
			fn = kprobe1
		case 2:
			fn = kprobe2
		case 3:
			fn = kprobe3
		case 4:
			fn = kprobe4
		case 5:
			fn = kprobe5
		default:
			ignored += 1
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		kp, err := link.Kprobe(name, fn)
		bar.Increment()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.Fatalf("Opening kprobe %s: %s\n", name, err)
			} else {
				ignored += 1
			}
		} else {
			defer kp.Close()
		}
	}
	bar.Finish()
	fmt.Printf("Attached (ignored %d)\n", ignored)

	rd, err := perf.NewReader(events, os.Getpagesize())
	if err != nil {
		log.Fatalf("Creating perf event reader: %s", err)
	}
	defer rd.Close()

	go func() {
		<-ctx.Done()

		if err := rd.Close(); err != nil {
			log.Fatalf("Closing perf event reader: %s", err)
		}
	}()

	log.Println("Listening for events..")

	output := pwru.NewOutput(&flags, printSkbMap, addr2name)
	output.PrintHeader()

	var event pwru.Event
	for {
		record, err := rd.Read()
		if err != nil {
			if perf.IsClosed(err) {
				return
			}
			log.Printf("Reading from perf event reader: %s", err)
		}

		if record.LostSamples != 0 {
			log.Printf("Perf event ring buffer full, dropped %d samples", record.LostSamples)
			continue
		}

		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("Parsing perf event: %s", err)
			continue
		}

		output.Print(&event)

		select {
		case <-ctx.Done():
			break
		default:
			continue
		}
	}
}
