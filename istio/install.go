package istio

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"

	"github.com/layer5io/meshery-adapter-library/adapter"
	"github.com/layer5io/meshery-adapter-library/status"
	"github.com/layer5io/meshery-istio/internal/config"
	mesherykube "github.com/layer5io/meshkit/utils/kubernetes"
)

func (istio *Istio) installIstio(del bool, version, namespace string) (string, error) {
	istio.Log.Info(fmt.Sprintf("Requested install of version: %s", version))
	istio.Log.Info(fmt.Sprintf("Requested action is delete: %v", del))
	istio.Log.Info(fmt.Sprintf("Requested action is in namespace: %s", namespace))

	// Overiding the namespace to be empty
	// This is intentional as deploying istio on custom namespace
	// is a bit tricky
	namespace = ""
	istio.Log.Debug(fmt.Sprintf("Overidden namespace: %s", namespace))

	st := status.Installing

	if del {
		st = status.Removing
	}

	err := istio.Config.GetObject(adapter.MeshSpecKey, istio)
	if err != nil {
		return st, ErrMeshConfig(err)
	}

	manifest, err := istio.fetchManifest(version, del)
	if err != nil {
		istio.Log.Error(ErrInstallIstio(err))
		return st, ErrInstallIstio(err)
	}

	err = istio.applyManifest([]byte(manifest), del, namespace)
	if err != nil {
		istio.Log.Error(ErrInstallIstio(err))
		return st, ErrInstallIstio(err)
	}

	if del {
		return status.Removed, nil
	}
	return status.Installed, nil
}

func (istio *Istio) fetchManifest(version string, isDel bool) (string, error) {
	var (
		out bytes.Buffer
		er  bytes.Buffer
	)

	Executable, err := istio.getExecutable(version)
	if err != nil {
		return "", ErrFetchManifest(err, err.Error())
	}
	execCmd := []string{"install", "--set", "profile=demo", "-y"}
	if isDel {
		execCmd = []string{"x", "uninstall", "--purge", "-y"}
	}

	// We need a variable executable here hence using nosec
	// #nosec
	command := exec.Command(Executable, execCmd...)
	command.Stdout = &out
	command.Stderr = &er
	err = command.Run()
	if err != nil {
		return "", ErrFetchManifest(err, er.String())
	}

	return out.String(), nil
}

func (istio *Istio) applyManifest(contents []byte, isDel bool, namespace string) error {
	kclient, err := mesherykube.New(istio.KubeClient, istio.RestConfig)
	if err != nil {
		return err
	}

	err = kclient.ApplyManifest(contents, mesherykube.ApplyOptions{Namespace: namespace, Delete: isDel})
	if err != nil {
		return err
	}

	return nil
}

// getExecutable looks for the executable in
// 1. $PATH
// 2. Root config path
//
// If it doesn't find the executable in the path then it proceeds
// to download the binary from github releases and installs it
// in the root config path
func (istio *Istio) getExecutable(release string) (string, error) {
	const binaryName = "istioctl"
	alternateBinaryName := "istioctl-" + release

	// Look for the executable in the path
	istio.Log.Info("Looking for istio in the path...")
	executable, err := exec.LookPath(binaryName)
	if err == nil {
		return executable, nil
	}
	executable, err = exec.LookPath(alternateBinaryName)
	if err == nil {
		return executable, nil
	}

	// Look for config in the root path
	binPath := path.Join(config.RootPath(), "bin")
	istio.Log.Info("Looking for istio in", binPath, "...")
	executable = path.Join(binPath, alternateBinaryName)
	if _, err := os.Stat(executable); err == nil {
		return executable, nil
	}

	// Proceed to download the binary in the config root path
	istio.Log.Info("istio not found in the path, downloading...")
	res, err := downloadBinary(runtime.GOOS, runtime.GOARCH, release)
	if err != nil {
		return "", err
	}
	// Install the binary
	istio.Log.Info("Installing...")
	if err = installBinary(path.Join(binPath, alternateBinaryName), runtime.GOOS, res); err != nil {
		return "", err
	}
	if err := extractAndClean(binPath, alternateBinaryName, runtime.GOOS); err != nil {
		return "", err
	}

	istio.Log.Info("Done")
	return path.Join(binPath, alternateBinaryName), nil
}

func downloadBinary(platform, arch, release string) (*http.Response, error) {
	var url = "https://github.com/istio/istio/releases/download"
	switch platform {
	case "darwin":
		url = fmt.Sprintf("%s/%s/istioctl-%s-osx.tar.gz", url, release, release)
	case "windows":
		url = fmt.Sprintf("%s/%s/istioctl-%s-win.zip", url, release, release)
	case "linux":
		url = fmt.Sprintf("%s/%s/istioctl-%s-%s-%s.tar.gz", url, release, release, platform, arch)
	}

	resp, err := http.Get(url)
	if err != nil {
		return nil, ErrDownloadBinary(err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, ErrDownloadBinary(fmt.Errorf("bad status: %s", resp.Status))
	}

	return resp, nil
}

func installBinary(location, platform string, res *http.Response) error {
	// Close the response body
	defer func() {
		if err := res.Body.Close(); err != nil {
			fmt.Println(err)
		}
	}()

	err := os.MkdirAll(location, 0750)
	if err != nil {
		return err
	}

	switch platform {
	case "darwin":
		fallthrough
	case "linux":
		if err := tarxzf(location, res.Body); err != nil {
			return ErrInstallBinary(err)
		}
	case "windows":
		if err := unzip(location, res.Body); err != nil {
			return ErrInstallBinary(err)
		}
	}
	return nil
}

func tarxzf(location string, stream io.Reader) error {
	uncompressedStream, err := gzip.NewReader(stream)
	if err != nil {
		return err
	}

	tarReader := tar.NewReader(uncompressedStream)

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			return ErrTarXZF(err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// File traversal is required to store the binary at the right place
			// #nosec
			if err := os.MkdirAll(path.Join(location, header.Name), 0750); err != nil {
				return ErrTarXZF(err)
			}
		case tar.TypeReg:
			// File traversal is required to store the binary at the right place
			// #nosec
			outFile, err := os.Create(path.Join(location, header.Name))
			if err != nil {
				return ErrTarXZF(err)
			}
			// Trust istioctl tar
			// #nosec
			if _, err := io.Copy(outFile, tarReader); err != nil {
				return ErrTarXZF(err)
			}
			if err = outFile.Close(); err != nil {
				return ErrTarXZF(err)
			}

		default:
			return ErrTarXZF(err)
		}
	}

	return nil
}

func unzip(location string, zippedContent io.Reader) error {
	// Keep file in memory: Approx size ~ 50MB
	// TODO: Find a better approach
	zipped, err := ioutil.ReadAll(zippedContent)

	zReader, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return ErrUnzipFile(err)
	}

	for _, file := range zReader.File {
		zippedFile, err := file.Open()
		if err != nil {
			return ErrUnzipFile(err)
		}
		defer zippedFile.Close()

		extractedFilePath := path.Join(location, file.Name)

		if file.FileInfo().IsDir() {
			os.MkdirAll(extractedFilePath, file.Mode())
		} else {
			outputFile, err := os.OpenFile(
				extractedFilePath,
				os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
				file.Mode(),
			)
			if err != nil {
				return ErrUnzipFile(err)
			}
			defer outputFile.Close()

			_, err = io.Copy(outputFile, zippedFile)
			if err != nil {
				return ErrUnzipFile(err)
			}
		}
	}

	return nil
}

func extractAndClean(location, binName, platform string) error {
	platformSpecificName := "istioctl"
	if platform == "windows" {
		platformSpecificName += ".exe"
	}

	// Move binary to the right location
	err := os.Rename(path.Join(location, binName, platformSpecificName), path.Join(location, platformSpecificName))
	if err != nil {
		return err
	}

	// Cleanup
	if err = os.RemoveAll(path.Join(location, binName)); err != nil {
		return err
	}

	if platform == "windows" {
		binName += ".exe"
	}
	if err = os.Rename(path.Join(location, platformSpecificName), path.Join(location, binName)); err != nil {
		return err
	}

	switch platform {
	case "darwin":
		fallthrough
	case "linux":
		// Set permissions
		// Permsission has to be +x to be able to run the binary
		// #nosec
		if err = os.Chmod(path.Join(location, binName), 0750); err != nil {
			return err
		}
	}

	return nil
}
