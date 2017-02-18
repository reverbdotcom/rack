package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type BuildOptions struct {
	Cache       bool
	CacheDir    string
	Environment map[string]string
	Service     string
	Verbose     bool
}

func (m *Manifest) Build(dir, appName string, s Stream, opts BuildOptions) error {
	pulls := map[string][]string{}
	builds := []Service{}

	services, err := m.runOrder(opts.Service)
	if err != nil {
		return err
	}

	for _, service := range services {
		dockerFile := service.Build.Dockerfile
		if dockerFile == "" {
			dockerFile = service.Dockerfile
		}
		if image := service.Image; image != "" {
			// make the implicit :latest explicit for caching/pulling
			sp := strings.Split(image, "/")
			if !strings.Contains(sp[len(sp)-1], ":") {
				image = image + ":latest"
			}
			pulls[image] = append(pulls[image], service.Tag(appName))
		} else {
			builds = append(builds, service)
		}
	}

	buildCache := map[string]string{}

	for _, service := range builds {
		if bc, ok := buildCache[service.Build.Hash()]; ok {
			if err := DefaultRunner.Run(s, Docker("tag", bc, service.Tag(appName)), RunnerOptions{Verbose: opts.Verbose}); err != nil {
				return fmt.Errorf("build error: %s", err)
			}
			continue
		}

		args := []string{"build"}

		if !opts.Cache {
			args = append(args, "--no-cache")
		}

		context := filepath.Join(dir, coalesce(service.Build.Context, "."))
		dockerFile := coalesce(service.Dockerfile, "Dockerfile")
		dockerFile = coalesce(service.Build.Dockerfile, dockerFile)
		dockerFile = filepath.Join(context, dockerFile)

		if opts.CacheDir != "" {
			rcd := filepath.Join(opts.CacheDir, service.Build.Hash())
			lcd := filepath.Join(dir, ".cache", "build")

			_, err := os.Stat(rcd)
			if os.IsNotExist(err) {
				// remote cache doesn't exist, do nothing
			} else if err == nil {
				if err := os.RemoveAll(lcd); err != nil {
					s <- fmt.Sprintf("cache error: %s", err)
				}

				if err := copyDir(rcd, lcd); err != nil {
					// do not display "error" if dir doesn't exist
					if !strings.Contains(err.Error(), "no such file or directory") {
						s <- fmt.Sprintf("cache error: %s", err)
					}
				}
			}
		}

		bargs := map[string]string{}

		for k, v := range service.Build.Args {
			bargs[k] = v
		}

		dba, err := buildArgs(dockerFile)
		if err != nil {
			return err
		}

		for _, ba := range dba {
			if v, ok := opts.Environment[ba]; ok {
				bargs[ba] = v
			}
		}

		bargNames := []string{}

		for k := range bargs {
			bargNames = append(bargNames, k)
		}

		sort.Strings(bargNames)

		for _, name := range bargNames {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", name, bargs[name]))
		}

		args = append(args, "-f", dockerFile)
		args = append(args, "-t", service.Tag(appName))
		args = append(args, context)

		ropts := RunnerOptions{
			Verbose: opts.Verbose,
			StreamHandlers: []RunnerStreamHandler{
				func(str string) string {
					// do not display "error" if dir doesn't exist
					if strings.Contains(str, "/var/cache/build: no such file or directory") {
						return ""
					}
					return str
				},
			},
		}

		if err := DefaultRunner.Run(s, Docker(args...), ropts); err != nil {
			return fmt.Errorf("build error: %s", err)
		}

		if opts.CacheDir != "" {
			hash := service.Build.Hash()

			if err := DefaultRunner.Run(s, Docker("create", "--name", hash, service.Tag(appName)), ropts); err != nil {
				s <- fmt.Sprintf("cache error: %s", err)
			}

			exec.Command("rm", "-rf", filepath.Join(opts.CacheDir, hash)).Run()

			if err := DefaultRunner.Run(s, Docker("cp", fmt.Sprintf("%s:/var/cache/build", hash), filepath.Join(opts.CacheDir, hash)), ropts); err != nil {
				s <- fmt.Sprintf("ignoring build cache")
			}

			if err := DefaultRunner.Run(s, Docker("rm", hash), ropts); err != nil {
				s <- fmt.Sprintf("cache error: %s", err)
			}
		}

		buildCache[service.Build.Hash()] = service.Tag(appName)
	}

	for image, tags := range pulls {
		args := []string{"pull"}

		output, err := DefaultRunner.CombinedOutput(Docker("images", "-q", image))
		if err != nil {
			return err
		}

		args = append(args, image)

		if !opts.Cache || len(output) == 0 {
			if err := DefaultRunner.Run(s, Docker("pull", image), RunnerOptions{Verbose: opts.Verbose}); err != nil {
				return fmt.Errorf("build error: %s", err)
			}
		}
		for _, tag := range tags {
			if err := DefaultRunner.Run(s, Docker("tag", image, tag), RunnerOptions{Verbose: opts.Verbose}); err != nil {
				return fmt.Errorf("build error: %s", err)
			}
		}
	}

	return nil
}

func buildArgs(dockerfile string) ([]string, error) {
	args := []string{}

	data, err := ioutil.ReadFile(dockerfile)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))

	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())

		if len(parts) < 1 {
			continue
		}

		switch parts[0] {
		case "ARG":
			args = append(args, strings.SplitN(parts[1], "=", 2)[0])
		}
	}

	return args, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}

	err = out.Sync()
	if err != nil {
		return err
	}

	stat, err := os.Stat(src)
	if err != nil {
		return err
	}

	err = os.Chmod(dst, stat.Mode())
	if err != nil {
		return err
	}

	err = os.Chtimes(dst, stat.ModTime(), stat.ModTime())
	if err != nil {
		return err
	}

	return nil
}

func copyDir(src string, dst string) error {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)

	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !si.IsDir() {
		return fmt.Errorf("source is not a directory")
	}

	_, err = os.Stat(dst)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		return fmt.Errorf("destination already exists")
	}

	err = os.MkdirAll(dst, si.Mode())
	if err != nil {
		return err
	}

	files, err := ioutil.ReadDir(src)
	if err != nil {
		return err
	}

	for _, f := range files {
		srcPath := filepath.Join(src, f.Name())
		dstPath := filepath.Join(dst, f.Name())

		if f.IsDir() {
			err = copyDir(srcPath, dstPath)
			if err != nil {
				return err
			}
		} else {
			if f.Mode()&os.ModeSymlink != 0 {
				// ignore symlinks
				continue
			}

			err = copyFile(srcPath, dstPath)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
