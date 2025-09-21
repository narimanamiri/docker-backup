package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/kennygrant/sanitize"
	"github.com/spf13/cobra"
)

// Backup is used to gather all of a container's metadata, so we can encode it
// as JSON and store it
type Backup struct {
	Name    string
	Config  *container.Config
	PortMap nat.PortMap
	Mounts  []types.MountPoint
}

var (
	optLaunch  = ""
	optTar     = false
	optAll     = false
	optStopped = false
	optVerbose = false

	paths []string
	tw    *tar.Writer

	backupCmd = &cobra.Command{
		Use:   "backup [container-id]",
		Short: "creates a backup of a container",
		RunE: func(cmd *cobra.Command, args []string) error {
			if optAll {
				return backupAll()
			}

			if len(args) < 1 {
				return fmt.Errorf("backup requires the ID of a container")
			}
			return backup(args[0])
		},
	}
)

func collectFile(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	if optVerbose {
		fmt.Println("Adding", path)
	}

	paths = append(paths, path)
	return nil
}

func collectFileTar(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	
	// Skip special files that can cause issues
	if info.Mode()&os.ModeSocket != 0 || 
	   info.Mode()&os.ModeNamedPipe != 0 ||
	   info.Mode()&os.ModeDevice != 0 {
		return nil
	}

	if optVerbose {
		fmt.Println("Adding", path)
	}

	// Handle symlinks by getting the actual file info
	actualPath := path
	if info.Mode()&os.ModeSymlink != 0 {
		// Resolve symlink to get the actual file
		linkPath, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(linkPath) {
			linkPath = filepath.Join(filepath.Dir(path), linkPath)
		}
		actualPath = linkPath
		info, err = os.Stat(actualPath)
		if err != nil {
			return err
		}
	}

	th, err := tar.FileInfoHeader(info, path)
	if err != nil {
		return err
	}

	th.Name = path
	if si, ok := info.Sys().(*syscall.Stat_t); ok {
		th.Uid = int(si.Uid)
		th.Gid = int(si.Gid)
	}

	if err := tw.WriteHeader(th); err != nil {
		return err
	}

	// Only copy content for regular files
	if !info.Mode().IsRegular() {
		return nil
	}

	file, err := os.Open(actualPath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Use CopyN to ensure we only copy the exact number of bytes specified in the header
	_, err = io.CopyN(tw, file, info.Size())
	if err != nil && err != io.EOF {
		return err
	}

	return nil
}

func backupTar(filename string, backup Backup) error {
	b, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}

	tarfile, err := os.Create(filename + ".tar")
	if err != nil {
		return err
	}
	defer tarfile.Close()
	
	tw = tar.NewWriter(tarfile)
	defer tw.Close()

	th := &tar.Header{
		Name:       "container.json",
		Size:       int64(len(b)),
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
		Mode:       0600,
	}

	if err := tw.WriteHeader(th); err != nil {
		return err
	}
	if _, err := tw.Write(b); err != nil {
		return err
	}

	for _, m := range backup.Mounts {
		err := filepath.Walk(m.Source, collectFileTar)
		if err != nil {
			fmt.Printf("Warning: Error walking path %s: %v\n", m.Source, err)
			continue
		}
	}

	fmt.Println("Created backup:", filename+".tar")
	return nil
}

func getFullImageName(imageName string) (string, error) {
	if strings.Contains(imageName, ":") {
		return imageName, nil
	}

	images, err := cli.ImageList(ctx, types.ImageListOptions{})
	if err != nil {
		return imageName, err
	}

	for _, image := range images {
		if !strings.Contains(imageName, image.ID) || len(image.RepoTags) == 0 {
			continue
		}

		for _, tag := range image.RepoTags {
			if !strings.Contains(tag, imageName) {
				continue
			}
			return tag, nil
		}
		return image.RepoTags[0], nil
	}

	return imageName, nil
}

func backup(ID string) error {
	conf, err := cli.ContainerInspect(ctx, ID)
	if err != nil {
		return err
	}
	fmt.Printf("Creating backup of %s (%s, %s)\n", conf.Name[1:], conf.Config.Image, conf.ID[:12])

	paths = []string{}

	conf.Config.Image, err = getFullImageName(conf.Config.Image)
	if err != nil {
		return err
	}

	backup := Backup{
		Name:    conf.Name,
		PortMap: conf.HostConfig.PortBindings,
		Config:  conf.Config,
		Mounts:  conf.Mounts,
	}

	filename := sanitize.Path(fmt.Sprintf("%s-%s", conf.Config.Image, ID))
	filename = strings.Replace(filename, "/", "_", -1)
	if optTar {
		return backupTar(filename, backup)
	}

	b, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}

	err = os.WriteFile(filename+".backup.json", b, 0600)
	if err != nil {
		return err
	}

	for _, m := range conf.Mounts {
		err := filepath.Walk(m.Source, collectFile)
		if err != nil {
			fmt.Printf("Warning: Error walking path %s: %v\n", m.Source, err)
			continue
		}
	}

	filelist, err := os.Create(filename + ".backup.files")
	if err != nil {
		return err
	}
	defer filelist.Close()

	_, err = filelist.WriteString(filename + ".backup.json\n")
	if err != nil {
		return err
	}
	for _, s := range paths {
		_, err := filelist.WriteString(s + "\n")
		if err != nil {
			return err
		}
	}

	fmt.Println("Created backup:", filename+".backup.json")

	if optLaunch != "" {
		ol := strings.Replace(optLaunch, "%tag", filename, -1)
		ol = strings.Replace(ol, "%list", filename+".backup.files", -1)

		fmt.Println("Launching external command and waiting for it to finish:")
		fmt.Println(ol)

		l := strings.Split(ol, " ")
		cmd := exec.Command(l[0], l[1:]...)
		return cmd.Run()
	}

	return nil
}

func backupAll() error {
	containers, err := cli.ContainerList(ctx, types.ContainerListOptions{
		All: optStopped,
	})
	if err != nil {
		return err
	}

	for _, container := range containers {
		err := backup(container.ID)
		if err != nil {
			fmt.Printf("Warning: Failed to backup container %s: %v\n", container.ID, err)
			continue
		}
	}

	return nil
}

func init() {
	backupCmd.Flags().StringVarP(&optLaunch, "launch", "l", "", "launch external program with file-list as argument")
	backupCmd.Flags().BoolVarP(&optTar, "tar", "t", false, "create tar backups")
	backupCmd.Flags().BoolVarP(&optAll, "all", "a", false, "backup all running containers")
	backupCmd.Flags().BoolVarP(&optStopped, "stopped", "s", false, "in combination with --all: also backup stopped containers")
	backupCmd.Flags().BoolVarP(&optVerbose, "verbose", "v", false, "print detailed backup progress")
	RootCmd.AddCommand(backupCmd)
}
