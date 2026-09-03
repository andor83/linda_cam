// Package ortep picks an ONNX Runtime execution provider for the host it is
// running on: CoreML on macOS, CUDA elsewhere. Every failure path falls
// through to ONNX Runtime's default CPU provider, so a missing GPU, driver or
// provider library costs accuracy nothing and only costs speed.
package ortep

import (
	"log"
	"os"
	"runtime"
	"strings"

	ort "github.com/yalue/onnxruntime_go"
)

// Enable registers the best available hardware execution provider on opts,
// logging what happened under tag. Setting LINDA_DETECTOR_DEVICE=cpu skips
// hardware acceleration entirely.
func Enable(opts *ort.SessionOptions, tag string) {
	if strings.ToLower(os.Getenv("LINDA_DETECTOR_DEVICE")) == "cpu" {
		return
	}

	if runtime.GOOS == "darwin" {
		// Flags 0 is CoreML's default: it chooses between the Neural Engine
		// and the GPU per model, and hands operators it cannot take back to
		// the CPU provider node by node.
		if err := opts.AppendExecutionProviderCoreML(0); err != nil {
			log.Printf("%s: CoreML EP registration failed (%v); falling back to CPU", tag, err)
			return
		}
		log.Printf("%s: CoreML execution provider enabled", tag)
		return
	}

	cudaOpts, err := ort.NewCUDAProviderOptions()
	if err != nil {
		log.Printf("%s: CUDA provider options unavailable (%v); falling back to CPU", tag, err)
		return
	}
	defer func() { _ = cudaOpts.Destroy() }()

	if err := opts.AppendExecutionProviderCUDA(cudaOpts); err != nil {
		log.Printf("%s: CUDA EP registration failed (%v); falling back to CPU", tag, err)
		return
	}
	log.Printf("%s: CUDA execution provider enabled", tag)
}
