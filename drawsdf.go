package ui

import (
	"context"
	"github.com/Yeicor/sdfx-ui/internal"
	"github.com/deadsy/sdfx/sdf"
	"github.com/hajimehoshi/ebiten"
	"image"
	"image/color"
	"log"
	"math"
	"runtime/debug"
	"time"
)

func (r *Renderer) drawSDF(screen *ebiten.Image) {
	// Draw latest SDF render (and overlay the latest partial render)
	r.implStateLock.RLock()
	defer r.implStateLock.RUnlock()
	r.cachedRenderLock.RLock()
	defer r.cachedRenderLock.RUnlock()
	drawOpts := &ebiten.DrawImageOptions{}
	var tr sdf.V2
	if r.translateFrom[0] != math.MaxInt && !r.smoothCamera { // Preview translations without rendering (until mouse release)
		cx, cy := getCursor()
		if r.translateFromStop[0] != math.MaxInt {
			cx, cy = r.translateFromStop[0], r.translateFromStop[1]
		}
		tr = sdf.V2i{cx, cy}.ToV2().Sub(r.translateFrom.ToV2()).DivScalar(float64(r.implState.ResInv))
		// TODO: Place SDF2 render at the right location during special renders (zooming, changing resolution)
	}
	drawOpts.GeoM.Translate(tr.X, tr.Y)
	cachedRenderWidth, cachedRenderHeight := r.cachedRender.Size()
	drawOpts.GeoM.Scale(float64(r.screenSize[0])/float64(cachedRenderWidth), float64(r.screenSize[1])/float64(cachedRenderHeight))
	err := screen.DrawImage(r.cachedRender, drawOpts)
	if err != nil {
		panic(err) // Can this happen?
	}
	drawOpts.GeoM.Reset()
	cachedPartialRenderWidth, cachedPartialRenderHeight := r.cachedPartialRender.Size()
	drawOpts.GeoM.Scale(float64(r.screenSize[0])/float64(cachedPartialRenderWidth), float64(r.screenSize[1])/float64(cachedPartialRenderHeight))
	err = screen.DrawImage(r.cachedPartialRender, drawOpts)
	if err != nil {
		panic(err) // Can this happen?
	}
}

// rerender will discard any current rendering and start a new render (use it when something changes).
// It does not lock execution (queues a new render in background).
func (r *Renderer) rerender(callbacks ...func(err error)) {
	r.rerenderOpt(true, callbacks...)
}

func (r *Renderer) rerenderOpt(forceCancel bool, callbacks ...func(err error)) {
	r.cachedRenderLock.RLock()
	if r.cachedRender == nil {
		log.Println("Trying to render too soon (before first Update())")
		debug.PrintStack()
	}
	r.cachedRenderLock.RUnlock()
	go func(callbacks ...func(err error)) {
		var err error
		defer func() {
			for _, callback := range callbacks {
				callback(err)
			}
		}()
		//lint:ignore SA1012 package API allows this
		if !r.renderingLock.TryLock(nil) {
			if !forceCancel {
				return
			}
			r.implStateLock.RLock() // Avoid race condition with creating a new context
			r.renderingCtxCancel()
			r.implStateLock.RUnlock()
			r.renderingLock.Lock() // Wait for previous render to finish (should be very fast)
		}
		defer r.renderingLock.Unlock()
		var renderCtx context.Context
		r.implStateLock.Lock()
		renderCtx, r.renderingCtxCancel = context.WithCancel(context.Background())
		renderSize := r.screenSize.ToV2().DivScalar(float64(r.implState.ResInv)).ToV2i()
		r.implStateLock.Unlock()
		partialRenders := make(chan *image.RGBA)
		go func(renderSize sdf.V2i) {
			partialRenderCopy := image.NewRGBA(image.Rect(0, 0, renderSize[0], renderSize[1]))
			lastPartialRender := time.Now()
			for partialRender := range partialRenders {
				if time.Since(lastPartialRender) < r.partialRenderEvery {
					continue // Skip this partial render (throttled) as it slows down significantly the full render
				}
				lastPartialRender = time.Now()
				r.cachedRenderLock.RLock()
				copy(partialRenderCopy.Pix, partialRender.Pix)
				r.cachedRenderLock.RUnlock()
				// WARNING: This blocks the main rendering thread: call sparingly
				gpuImg, err := ebiten.NewImageFromImage(partialRenderCopy, ebiten.FilterDefault)
				if err != nil {
					log.Println("Error sending image to GPU:", err)
					continue
				}
				r.cachedRenderLock.Lock()
				r.cachedPartialRender = gpuImg
				r.cachedRenderLock.Unlock()
			}
			r.cachedRenderLock.Lock() // Use the cached render as the partial one (to make sure it is complete)
			err := r.cachedPartialRender.Fill(color.Transparent)
			if err != nil {
				log.Println("cachedPartialRender.Fill(color.Transparent) error:", err)
			}
			r.cachedRenderLock.Unlock()
		}(renderSize)
		renderStartTime := time.Now()
		r.implStateLock.RLock()
		sameSize := r.cachedRenderCPU != nil && (sdf.V2i{r.cachedRenderCPU.Rect.Max.X, r.cachedRenderCPU.Rect.Max.Y} == renderSize)
		if !sameSize {
			r.cachedRenderCPU = image.NewRGBA(image.Rect(0, 0, renderSize[0], renderSize[1]))
		}
		implState := r.implState
		if r.smoothCamera {
			if r.translateFrom[0] != math.MaxInt {
				cx, cy := getCursor()
				switch r.implDimCache {
				case 2:
					implState = r.apply2DCameraMoveTo(cx, cy)
				case 3:
					implState = r.apply3DCameraMoveTo(cx, cy)
				}
			}
		}
		r.implStateLock.RUnlock()
		r.implLock.RLock()
		err = r.impl.Render(&internal.RenderArgs{
			Ctx:              renderCtx,
			State:            implState,
			StateLock:        r.implStateLock,
			CachedRenderLock: r.cachedRenderLock,
			PartialRenders:   partialRenders,
			FullRender:       r.cachedRenderCPU,
		})
		r.implLock.RUnlock()
		if err != nil {
			if err != context.Canceled {
				log.Println("[DevRenderer] Error rendering:", err)
			}
			return
		}
		renderGPUStartTime := time.Now()
		renderGpuImg, err := ebiten.NewImageFromImage(r.cachedRenderCPU, ebiten.FilterDefault)
		if err != nil {
			log.Println("Error sending image to GPU:", err)
			return
		}
		log.Println("[DevRenderer] CPU Render took:", renderGPUStartTime.Sub(renderStartTime), "- Sending to GPU took:", time.Since(renderGPUStartTime))
		r.implLock.RLock()
		r.implStateLock.Lock()               // WARNING: Locking order (to avoid deadlocks)
		r.implDimCache = r.impl.Dimensions() // Only updated here
		if r.implDimCache == 2 {
			r.cachedRenderBb2 = r.implState.Bb
		} else {
			r.cachedRenderBb2 = sdf.Box2{}
		}
		r.implStateLock.Unlock()
		r.implLock.RUnlock()
		r.cachedRenderLock.Lock()
		// Need to resize the rendering result: overwrite
		r.cachedRender = renderGpuImg
		// TODO: reuse the previous render for the parts that did not change (SDF2 only)
		r.cachedRenderLock.Unlock()
	}(callbacks...)
}
