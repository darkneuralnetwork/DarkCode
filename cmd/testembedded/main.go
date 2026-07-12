package main

import (
    "context"
    "fmt"
    "time"

    "github.com/darkcode/provider/embedded"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
    defer cancel()

    prov := embedded.NewProviderWithDirs(nil, "./models", ".darkcode/bin")
    if err := prov.LoadModel(ctx, "qwen2_5-1_5b-instruct-q4_k_m.gguf"); err != nil {
        fmt.Println("LoadModel error:", err)
        return
    }
    fmt.Println("Loaded model; status:", prov.Status())
    // Let the server run briefly so any stderr is emitted/captured by the
    // spawned process before we shut it down.
    time.Sleep(5 * time.Second)
    prov.UnloadModel()
    fmt.Println("Stopped provider")
}
