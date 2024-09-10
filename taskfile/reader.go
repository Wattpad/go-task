package taskfile

import (
	"fmt"
	"os"
	"time"

	"github.com/dominikbraun/graph"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/go-task/task/v3/errors"
	"github.com/go-task/task/v3/internal/compiler"
	"github.com/go-task/task/v3/internal/filepathext"
	"github.com/go-task/task/v3/internal/logger"
	"github.com/go-task/task/v3/internal/templater"
	"github.com/go-task/task/v3/taskfile/ast"
)

const (
	taskfileUntrustedPrompt = `The task you are attempting to run depends on the remote Taskfile at %q.
--- Make sure you trust the source of this Taskfile before continuing ---
Continue?`
	taskfileChangedPrompt = `The Taskfile at %q has changed since you last used it!
--- Make sure you trust the source of this Taskfile before continuing ---
Continue?`
)

// A Reader will recursively read Taskfiles from a given source using a directed
// acyclic graph (DAG).
type Reader struct {
	graph    *ast.TaskfileGraph
	node     Node
	insecure bool
	download bool
	offline  bool
	timeout  time.Duration
	tempDir  string
	logger   *logger.Logger
}

func NewReader(
	node Node,
	insecure bool,
	download bool,
	offline bool,
	timeout time.Duration,
	tempDir string,
	logger *logger.Logger,
) *Reader {
	return &Reader{
		graph:    ast.NewTaskfileGraph(),
		node:     node,
		insecure: insecure,
		download: download,
		offline:  offline,
		timeout:  timeout,
		tempDir:  tempDir,
		logger:   logger,
	}
}

func (r *Reader) Read() (*ast.TaskfileGraph, error) {
	// Recursively loop through each Taskfile, adding vertices/edges to the graph
	if err := r.include(r.node); err != nil {
		return nil, err
	}

	return r.graph, nil
}

func (r *Reader) include(node Node) error {
	// Create a new vertex for the Taskfile
	vertex := &ast.TaskfileVertex{
		URI:      node.Location(),
		Taskfile: nil,
	}

	// Add the included Taskfile to the DAG
	// If the vertex already exists, we return early since its Taskfile has
	// already been read and its children explored
	if err := r.graph.AddVertex(vertex); err == graph.ErrVertexAlreadyExists {
		return nil
	} else if err != nil {
		return err
	}

	// Read and parse the Taskfile from the file and add it to the vertex
	var err error
	vertex.Taskfile, node, err = r.readNode(node)
	if err != nil {
		return err
	}

	// Create an error group to wait for all included Taskfiles to be read
	var g errgroup.Group

	// Loop over each included taskfile
	_ = vertex.Taskfile.Includes.Range(func(namespace string, include *ast.Include) error {
		vars := compiler.GetEnviron()
		vars.Merge(vertex.Taskfile.Vars, nil)
		// Start a goroutine to process each included Taskfile
		g.Go(func() error {
			cache := &templater.Cache{Vars: vars}
			include = &ast.Include{
				Namespace:      include.Namespace,
				Taskfile:       templater.Replace(include.Taskfile, cache),
				Dir:            templater.Replace(include.Dir, cache),
				Optional:       include.Optional,
				Internal:       include.Internal,
				Flatten:        include.Flatten,
				Aliases:        include.Aliases,
				AdvancedImport: include.AdvancedImport,
				Vars:           include.Vars,
			}
			if err := cache.Err(); err != nil {
				return err
			}

			entrypoint, err := node.ResolveEntrypoint(include.Taskfile)
			if err != nil {
				return err
			}

			include.Dir, err = node.ResolveDir(include.Dir)
			if err != nil {
				return err
			}

			includeNode, err := NewNode(r.logger, entrypoint, include.Dir, r.insecure, r.timeout,
				WithParent(node),
			)
			if err != nil {
				if include.Optional {
					return nil
				}
				return err
			}

			// Recurse into the included Taskfile
			if err := r.include(includeNode); err != nil {
				return err
			}

			// Create an edge between the Taskfiles
			r.graph.Lock()
			defer r.graph.Unlock()
			edge, err := r.graph.Edge(node.Location(), includeNode.Location())
			if err == graph.ErrEdgeNotFound {
				// If the edge doesn't exist, create it
				err = r.graph.AddEdge(
					node.Location(),
					includeNode.Location(),
					graph.EdgeData([]*ast.Include{include}),
					graph.EdgeWeight(1),
				)
			} else {
				// If the edge already exists
				edgeData := append(edge.Properties.Data.([]*ast.Include), include)
				err = r.graph.UpdateEdge(
					node.Location(),
					includeNode.Location(),
					graph.EdgeData(edgeData),
					graph.EdgeWeight(len(edgeData)),
				)
			}
			if errors.Is(err, graph.ErrEdgeCreatesCycle) {
				return errors.TaskfileCycleError{
					Source:      node.Location(),
					Destination: includeNode.Location(),
				}
			}
			return err
		})
		return nil
	})

	// Wait for all the go routines to finish
	return g.Wait()
}

func (r *Reader) readNode(node Node) (*ast.Taskfile, Node, error) {
	node, err := r.loadNode(node)
	if err != nil {
		return nil, nil, err
	}

	src, err := node.Read()
	if err != nil {
		return nil, nil, err
	}

	var tf ast.Taskfile
	if err := yaml.Unmarshal(src.FileContent, &tf); err != nil {
		// Decode the taskfile and add the file info the any errors
		taskfileInvalidErr := &errors.TaskfileDecodeError{}
		if errors.As(err, &taskfileInvalidErr) {
			return nil, nil, taskfileInvalidErr.WithFileInfo(node.Location(), src.FileContent, 2)
		}
		return nil, nil, &errors.TaskfileInvalidError{URI: filepathext.TryAbsToRel(node.Location()), Err: err}
	}

	// Check that the Taskfile is set and has a schema version
	if tf.Version == nil {
		return nil, nil, &errors.TaskfileVersionCheckError{URI: node.Location()}
	}

	// Set the taskfile/task's locations
	tf.Location = node.Location()
	for _, task := range tf.Tasks.Values() {
		// If the task is not defined, create a new one
		if task == nil {
			task = &ast.Task{}
		}
		// Set the location of the taskfile for each task
		if task.Location.Taskfile == "" {
			task.Location.Taskfile = tf.Location
		}
		if task.Location.TaskfileDir == "" {
			task.Location.TaskfileDir = src.FileDirectory
		}
	}

	return &tf, node, nil
}

func (r *Reader) loadNode(n Node) (Node, error) {
	remote, ok := n.(*RemoteNode)
	if !ok {
		return n, nil
	}

	cache, err := NewCache(r.tempDir)
	if err != nil {
		return nil, err
	}

	if r.offline {
		// In offline mode try to use cached copy
		cached, err := cache.read(*remote)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &errors.TaskfileCacheNotFoundError{URI: remote.Location()}
		} else if err != nil {
			return nil, err
		}
		r.logger.VerboseOutf(logger.Magenta, "task: [%s] Fetched cached copy\n", remote.Location())

		return cached, nil
	}

	src, err := remote.Read()

	var te errors.TaskfileNetworkTimeoutError
	if errors.As(err, &te) {
		// If we timed out then we likely have a network issue

		// If a download was requested, then we can't use a cached copy
		if r.download {
			return nil, &errors.TaskfileNetworkTimeoutError{URI: remote.Location(), Timeout: te.Timeout}
		}

		// Search for any cached copies
		cached, err := cache.read(*remote)
		if errors.Is(err, os.ErrNotExist) {
			return nil, &errors.TaskfileNetworkTimeoutError{URI: remote.Location(), Timeout: r.timeout, CheckedCache: true}
		} else if err != nil {
			return nil, err
		}
		r.logger.VerboseOutf(logger.Magenta, "task: [%s] Network timeout. Fetched cached copy\n", remote.Location())

		return cached, nil

	} else if err != nil {
		return nil, err
	}
	r.logger.VerboseOutf(logger.Magenta, "task: [%s] Fetched remote copy\n", remote.Location())

	// Get the checksums
	cachedChecksum := cache.readChecksum(*remote)
	checksum, err := checksumSource(*src)
	if err != nil {
		return nil, err
	}

	var prompt string
	if cachedChecksum == "" {
		// If the checksum doesn't exist, prompt the user to continue
		prompt = fmt.Sprintf(taskfileUntrustedPrompt, remote.Location())
	} else if checksum != cachedChecksum {
		// If there is a cached hash, but it doesn't match the expected hash, prompt the user to continue
		prompt = fmt.Sprintf(taskfileChangedPrompt, remote.Location())
	}

	if prompt != "" {
		if err := r.logger.Prompt(logger.Yellow, prompt, "n", "y", "yes"); err != nil {
			return nil, &errors.TaskfileNotTrustedError{URI: remote.Location()}
		}

		// Cache the file
		r.logger.VerboseOutf(logger.Magenta, "task: [%s] Caching downloaded file\n", remote.Location())
		if remote, err = cache.write(*remote, *src); err != nil {
			return nil, err
		}
	}

	return remote, nil
}
