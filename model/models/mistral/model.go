package llama

import (
	"fmt"
	"math"
	"strings"

	"github.com/ollama/ollama/kvcache"
	"github.com/ollama/ollama/ml"
	"github.com/ollama/ollama/ml/nn"
	"github.com/ollama/ollama/model"
	"github.com/ollama/ollama/model/input"
)

type Options struct {
	hiddenSize, numHeads, numKVHeads, headDim int
	eps, ropeBase, ropeScale                  float32
	ropeDim                                   uint32
}

type Model struct {
	model.Base
	model.BytePairEncoding

	TokenEmbedding *nn.Embedding `gguf:"token_embd"`
	Layers         []Layer       `gguf:"blk"`
	OutputNorm     *nn.RMSNorm   `gguf:"output_norm"`
	Output         *nn.Linear    `gguf:"output,alt:token_embd"`

	*Options
}

func New(c ml.Config) (model.Model, error) {
	if !strings.EqualFold(c.String("tokenizer.ggml.model"), "gpt2") {
		return nil, fmt.Errorf("tokenizer %s not yet supported", c.String("tokenizer.ggml.model"))
	}

	m := Model{
		BytePairEncoding: model.NewBytePairEncoding(
			c.String("tokenizer.ggml.pretokenizer", `[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*|\p{N}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+`),
			&model.Vocabulary{
				Values: c.Strings("tokenizer.ggml.tokens"),
				Types:  c.Uints("tokenizer.ggml.token_type"),
				Merges: c.Strings("tokenizer.ggml.merges"),
				BOS:    int32(c.Uint("tokenizer.ggml.bos_token_id")),
				AddBOS: c.Bool("tokenizer.ggml.add_bos_token", true),
				EOS:    int32(c.Uint("tokenizer.ggml.eos_token_id")),
				AddEOS: c.Bool("tokenizer.ggml.add_eos_token", false),
			},
		),
		Layers: make([]Layer, c.Uint("block_count")),
		Options: &Options{
			hiddenSize: int(c.Uint("embedding_length")),
			numHeads:   int(c.Uint("attention.head_count")),
			numKVHeads: int(c.Uint("attention.head_count_kv")),
			headDim:    int(c.Uint("attention.key_length")),
			eps:        c.Float("attention.layer_norm_rms_epsilon"),
			ropeBase:   c.Float("rope.freq_base"),
			ropeScale:  c.Float("rope.freq_scale", 1),
			ropeDim:    c.Uint("rope.dimension_count"),
		},
	}

	fmt.Println("Model Parameters:")
	fmt.Printf("  model_type: %q\n", "gpt2")
	fmt.Printf("  vocab_size: %d\n", len(c.Strings("tokenizer.ggml.tokens")))
	fmt.Printf("  hidden_size: %d\n", m.Options.hiddenSize)
	fmt.Printf("  num_hidden_layers: %d\n", c.Uint("block_count"))
	fmt.Printf("  num_attention_heads: %d\n", m.Options.numHeads)
	fmt.Printf("  num_key_value_heads: %d\n", m.Options.numKVHeads)
	fmt.Printf("  rms_norm_eps: %g\n", m.Options.eps)
	fmt.Printf("  rope_theta: %g\n", m.Options.ropeBase)
	fmt.Printf("  bos_token_id: %d\n", c.Uint("tokenizer.ggml.bos_token_id"))
	fmt.Printf("  eos_token_id: %d\n", c.Uint("tokenizer.ggml.eos_token_id"))
	fmt.Printf("  pad_token_id: %d\n", c.Uint("tokenizer.ggml.pad_token_id", 0))

	m.Cache = kvcache.NewCausalCache(m.Shift)

	return &m, nil
}

type SelfAttention struct {
	Query       *nn.Linear `gguf:"self_attn.q_proj"`
	Key         *nn.Linear `gguf:"self_attn.k_proj"`
	Value       *nn.Linear `gguf:"self_attn.v_proj"`
	Output      *nn.Linear `gguf:"self_attn.o_proj"`
	RopeFactors ml.Tensor  `gguf:"rope_freqs.weight"`
}

func (sa *SelfAttention) Forward(ctx ml.Context, hiddenState, positionIDs ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	batchSize := hiddenState.Dim(1)
	ropeType := uint32(0)
	// Get head dimension - use explicit value if available, otherwise calculate
	headDim := opts.headDim
	if headDim == 0 {
		headDim = opts.hiddenSize / opts.numHeads
	}

	// Query projection and reshape
	q := sa.Query.Forward(ctx, hiddenState)
	q = q.Reshape(ctx, headDim, opts.numHeads, batchSize)
	q = q.RoPE(ctx, positionIDs, sa.RopeFactors, opts.ropeDim, ropeType, opts.ropeBase, opts.ropeScale)

	// Key projection and reshape
	k := sa.Key.Forward(ctx, hiddenState)
	k = k.Reshape(ctx, headDim, opts.numKVHeads, batchSize)
	k = k.RoPE(ctx, positionIDs, sa.RopeFactors, opts.ropeDim, ropeType, opts.ropeBase, opts.ropeScale)

	// Value projection and reshape
	v := sa.Value.Forward(ctx, hiddenState)
	v = v.Reshape(ctx, headDim, opts.numKVHeads, batchSize)

	// Attention computation
	scaleFactor := 1.0 / math.Sqrt(float64(headDim))
	kqv := nn.Attention(ctx, q, k, v, scaleFactor, cache)

	// Reshape attention output for final projection
	outputDim := headDim * opts.numHeads
	kqv = kqv.Reshape(ctx, outputDim, batchSize)

	// Apply output projection
	return sa.Output.Forward(ctx, kqv)
}

func (m *Model) Shift(ctx ml.Context, layer int, key, shift ml.Tensor) (ml.Tensor, error) {
	return key.RoPE(ctx, shift, m.Layers[layer].SelfAttention.RopeFactors, uint32(0), m.ropeDim, m.ropeBase, m.ropeScale), nil
}

type MLP struct {
	Up   *nn.Linear `gguf:"mlp.up_proj"`
	Down *nn.Linear `gguf:"mlp.down_proj"`
	Gate *nn.Linear `gguf:"mlp.gate_proj"`
}

func (mlp *MLP) Forward(ctx ml.Context, hiddenState ml.Tensor, opts *Options) ml.Tensor {
	hiddenState = mlp.Gate.Forward(ctx, hiddenState).SILU(ctx).Mul(ctx, mlp.Up.Forward(ctx, hiddenState))
	return mlp.Down.Forward(ctx, hiddenState)
}

type Layer struct {
	AttentionNorm *nn.RMSNorm `gguf:"input_layernorm"`
	SelfAttention *SelfAttention
	MLPNorm       *nn.RMSNorm `gguf:"post_attention_layernorm"`
	MLP           *MLP
}

func (l *Layer) Forward(ctx ml.Context, hiddenState, positionIDs, outputs ml.Tensor, cache kvcache.Cache, opts *Options) ml.Tensor {
	residual := hiddenState

	hiddenState = l.AttentionNorm.Forward(ctx, hiddenState, opts.eps)
	hiddenState = l.SelfAttention.Forward(ctx, hiddenState, positionIDs, cache, opts)

	// In the final layer (outputs != nil), optimize by pruning to just the token positions
	// we need logits for.
	if outputs != nil {
		hiddenState = hiddenState.Rows(ctx, outputs)
		residual = residual.Rows(ctx, outputs)
	}

	hiddenState = hiddenState.Add(ctx, residual)
	residual = hiddenState

	hiddenState = l.MLPNorm.Forward(ctx, hiddenState, opts.eps)
	hiddenState = l.MLP.Forward(ctx, hiddenState, opts)
	return hiddenState.Add(ctx, residual)
}

func (m *Model) Forward(ctx ml.Context, opts input.Options) (ml.Tensor, error) {
	inputs, err := ctx.Input().FromIntSlice(opts.Inputs, len(opts.Inputs))
	if err != nil {
		return nil, err
	}

	positions, err := ctx.Input().FromIntSlice(opts.Positions, len(opts.Positions))
	if err != nil {
		return nil, err
	}

	outputs, err := ctx.Output().FromIntSlice(opts.Outputs, len(opts.Outputs))
	if err != nil {
		return nil, err
	}

	// Get token embeddings
	hiddenState := m.TokenEmbedding.Forward(ctx, inputs)

	for i, layer := range m.Layers {
		m.Cache.SetLayer(i)

		var lastLayerOutputs ml.Tensor
		if i == len(m.Layers)-1 {
			lastLayerOutputs = outputs
		}

		hiddenState = layer.Forward(ctx, hiddenState, positions, lastLayerOutputs, m.Cache, m.Options)
	}

	// Apply output normalization
	hiddenState = m.OutputNorm.Forward(ctx, hiddenState, m.eps)

	// Apply output projection
	return m.Output.Forward(ctx, hiddenState), nil
}

func init() {
	model.Register("mistral", New)
}
