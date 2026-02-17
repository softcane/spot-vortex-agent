package inference

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/Knetic/govaluate"
)

type PySREngine struct {
	calibration *PySREquation
	fusion      *PySREquation
	logger      *slog.Logger
}

type PySREquation struct {
	expression *govaluate.EvaluableExpression
	vars       []string
	outputMin  float64
	outputMax  float64
}

func NewPySREngine(logger *slog.Logger, calibrationPath string, fusionPath string) *PySREngine {
	if logger == nil {
		logger = slog.Default()
	}
	engine := &PySREngine{logger: logger}

	if calibrationEnabled() {
		if eq, err := loadEquation(calibrationPath, 0.0, 1.0); err == nil {
			engine.calibration = eq
		} else {
			logger.Warn("failed to load calibration equation", "error", err)
		}
	}

	if fusionEnabled() {
		if eq, err := loadEquation(fusionPath, 0.0, 1.0); err == nil {
			engine.fusion = eq
		} else {
			logger.Warn("failed to load fusion equation", "error", err)
		}
	}

	return engine
}

func (p *PySREngine) ApplyCalibration(features map[string]float64) (float64, bool) {
	if p == nil || p.calibration == nil {
		return 0, false
	}
	value, err := p.calibration.Evaluate(features)
	if err != nil {
		p.logger.Warn("calibration evaluation failed", "error", err)
		return 0, false
	}
	return value, true
}

func (p *PySREngine) ApplyFusion(features map[string]float64) (float64, bool) {
	if p == nil || p.fusion == nil {
		return 0, false
	}
	value, err := p.fusion.Evaluate(features)
	if err != nil {
		p.logger.Warn("fusion evaluation failed", "error", err)
		return 0, false
	}
	return value, true
}

func (p *PySREngine) Enabled() bool {
	return (p != nil && (p.calibration != nil || p.fusion != nil))
}

var loadOnce sync.Once
var calibrationFlag bool
var fusionFlag bool

func calibrationEnabled() bool {
	loadOnce.Do(loadFlags)
	return calibrationFlag
}

func fusionEnabled() bool {
	loadOnce.Do(loadFlags)
	return fusionFlag
}

func loadFlags() {
	calibrationFlag = envFlag("VORTEX_ENABLE_PYSR_FEATURES", true)
	fusionFlag = envFlag("VORTEX_ENABLE_PYSR_FUSION", true)
}

func envFlag(name string, defaultValue bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return defaultValue
	}
}

func loadEquation(path string, outputMin, outputMax float64) (*PySREquation, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read equation: %w", err)
	}
	expr := strings.TrimSpace(string(payload))
	if expr == "" {
		return nil, fmt.Errorf("equation file empty: %s", path)
	}

	functions := map[string]govaluate.ExpressionFunction{
		"sqrt": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			if v < 0 {
				v = 0
			}
			return math.Sqrt(v), nil
		},
		"square": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			return v * v, nil
		},
		"exp": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			return math.Exp(v), nil
		},
		"log": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			if v <= 0 {
				v = 1e-6
			}
			return math.Log(v), nil
		},
		"sin": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			if v < -20 {
				v = -20
			} else if v > 20 {
				v = 20
			}
			return math.Sin(v), nil
		},
		"cos": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			if v < -20 {
				v = -20
			} else if v > 20 {
				v = 20
			}
			return math.Cos(v), nil
		},
		"tan": func(args ...interface{}) (interface{}, error) {
			v, err := toFloat(args, 0)
			if err != nil {
				return nil, err
			}
			if v < -1.4 {
				v = -1.4
			} else if v > 1.4 {
				v = 1.4
			}
			return math.Tan(v), nil
		},
	}

	evaluable, err := govaluate.NewEvaluableExpressionWithFunctions(expr, functions)
	if err != nil {
		return nil, fmt.Errorf("parse equation: %w", err)
	}

	return &PySREquation{
		expression: evaluable,
		vars:       evaluable.Vars(),
		outputMin:  outputMin,
		outputMax:  outputMax,
	}, nil
}

func (e *PySREquation) Evaluate(features map[string]float64) (float64, error) {
	params := make(map[string]interface{}, len(features))
	for _, key := range e.vars {
		value, ok := features[key]
		if !ok {
			return 0, fmt.Errorf("missing variable %q", key)
		}
		params[key] = value
	}

	result, err := e.expression.Evaluate(params)
	if err != nil {
		return 0, err
	}
	value, err := toFloatAny(result)
	if err != nil {
		return 0, err
	}

	if value < e.outputMin {
		value = e.outputMin
	} else if value > e.outputMax {
		value = e.outputMax
	}
	return value, nil
}

func toFloat(args []interface{}, idx int) (float64, error) {
	if len(args) <= idx {
		return 0, fmt.Errorf("missing argument %d", idx)
	}
	return toFloatAny(args[idx])
}

func toFloatAny(value interface{}) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case uint:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	case uint32:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", value)
	}
}
