package main

import (
    "context"
    "flag"
    "fmt"
    "log"
    "net/http"
    "os"
    "strconv"
    "time"

    "github.com/hackohio/agent/internal/agent"
)

func getenv(key, def string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return def
}

func main() {
    var (
        nodeName    = getenv("NODE_NAME", "")
        concurrency = getenv("AGENT_CONCURRENCY", "4")
        naGroup     = getenv("NA_GROUP", "risc.dev")
        naVersion   = getenv("NA_VERSION", "v1alpha1")
        naResource  = getenv("NA_RESOURCE", "nodeactions")
        isGroup     = getenv("IS_GROUP", naGroup)
        isVersion   = getenv("IS_VERSION", naVersion)
        isResource  = getenv("IS_RESOURCE", "instructionsets")
        metricsAddr = getenv("METRICS_ADDR", ":8080")
        driverAddr  = getenv("DRIVER_ADDR", "")
        driverInsec = getenv("DRIVER_INSECURE", "true")
    )

    // Allow overriding via flags as well (useful for local dev)
    flag.StringVar(&nodeName, "node-name", nodeName, "Kubernetes node name to watch for")
    flag.StringVar(&naGroup, "na-group", naGroup, "NodeAction API group")
    flag.StringVar(&naVersion, "na-version", naVersion, "NodeAction API version")
    flag.StringVar(&naResource, "na-resource", naResource, "NodeAction resource plural name")
    flag.StringVar(&isGroup, "is-group", isGroup, "InstructionSet API group")
    flag.StringVar(&isVersion, "is-version", isVersion, "InstructionSet API version")
    flag.StringVar(&isResource, "is-resource", isResource, "InstructionSet resource plural name")
    flag.StringVar(&metricsAddr, "metrics-addr", metricsAddr, "HTTP listen addr for metrics")
    flag.StringVar(&driverAddr, "driver-addr", driverAddr, "gRPC runtime driver address (e.g. unix:///var/run/runtime-driver.sock or host:port)")
    flag.StringVar(&driverInsec, "driver-insecure", driverInsec, "use insecure transport for driver (true/false)")
    flag.Parse()

    if nodeName == "" {
        log.Fatalf("NODE_NAME is required (use DownwardAPI).")
    }

    conc, err := strconv.Atoi(concurrency)
    if err != nil || conc <= 0 {
        conc = 4
    }
    insec := true
    if b, err := strconv.ParseBool(driverInsec); err == nil {
        insec = b
    }

    if driverAddr == "" {
        log.Fatalf("DRIVER_ADDR is required: gRPC runtime driver address must be set")
    }

    // Start metrics HTTP server
    go func() {
        mux := http.NewServeMux()
        mux.Handle("/metrics", agent.MetricsHandler())
        srv := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
        log.Printf("metrics listening on %s", metricsAddr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Printf("metrics server error: %v", err)
        }
    }()

    cfg := agent.Config{
        NodeName:                 nodeName,
        Concurrency:              conc,
        NodeActionGVR:            agent.GVR{Group: naGroup, Version: naVersion, Resource: naResource},
        InstructionSetGVR:        agent.GVR{Group: isGroup, Version: isVersion, Resource: isResource},
        DriverAddr:               driverAddr,
        DriverInsecure:           insec,
    }

    ag, err := agent.New(context.Background(), cfg)
    if err != nil {
        log.Fatalf("init agent: %v", err)
    }

    log.Printf("agent starting on node=%s, watching %s/%s %s", nodeName, naGroup, naVersion, naResource)
    if err := ag.Run(context.Background()); err != nil {
        log.Fatalf("agent stopped: %v", err)
    }
    fmt.Println("agent exited")
}
