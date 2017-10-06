// Copyright 2017 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus"
	"github.com/mendersoftware/mender-artifact/areader"
	"github.com/mendersoftware/mender-artifact/artifact"
	"github.com/mendersoftware/mender-artifact/awriter"
	"github.com/mendersoftware/mender-artifact/handlers"

	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

// Version of the mender-artifact CLI tool
var Version = "unknown"

// Latest version of the format, which is also what we default to.
const LatestFormatVersion = 2

func version(c *cli.Context) int {
	version := c.Int("version")
	return version
}

func artifactWriter(f *os.File, c *cli.Context,
	ver int) (*awriter.Writer, error) {
	if len(c.String("key")) != 0 {
		if ver == 1 {
			// check if we are having correct version
			return nil, errors.New("can not use signed artifact with version 1")
		}
		privateKey, err := getKey(c.String("key"))
		if err != nil {
			return nil, err
		}
		return awriter.NewWriterSigned(f, artifact.NewSigner(privateKey)), nil
	}
	return awriter.NewWriter(f), nil
}

func scripts(scripts []string) (*artifact.Scripts, error) {
	scr := artifact.Scripts{}
	for _, scriptArg := range scripts {
		statInfo, err := os.Stat(scriptArg)
		if err != nil {
			return nil, errors.Wrapf(err, "can not stat script file: %s", scriptArg)
		}

		// Read either a directory, or add the script file directly.
		if statInfo.IsDir() {
			fileList, err := ioutil.ReadDir(scriptArg)
			if err != nil {
				return nil, errors.Wrapf(err, "can not list directory contents of: %s", scriptArg)
			}
			for _, nameInfo := range fileList {
				if err := scr.Add(filepath.Join(scriptArg, nameInfo.Name())); err != nil {
					return nil, err
				}
			}
		} else {
			if err := scr.Add(scriptArg); err != nil {
				return nil, err
			}
		}
	}
	return &scr, nil
}

func writeArtifact(c *cli.Context) error {

	// set default name
	name := "artifact.mender"
	if len(c.String("output-path")) > 0 {
		name = c.String("output-path")
	}
	version := version(c)

	var h *handlers.Rootfs
	switch version {
	case 1:
		h = handlers.NewRootfsV1(c.String("update"))
	case 2:
		h = handlers.NewRootfsV2(c.String("update"))
	default:
		return cli.NewExitError("unsupported artifact version", 1)
	}

	upd := &awriter.Updates{
		U: []handlers.Composer{h},
	}

	f, err := os.Create(name + ".tmp")
	if err != nil {
		return cli.NewExitError("can not create artifact file", 1)
	}
	defer func() {
		f.Close()
		// in case of success `.tmp` suffix will be removed and below
		// will not remove valid artifact
		os.Remove(name + ".tmp")
	}()

	aw, err := artifactWriter(f, c, version)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	scr, err := scripts(c.StringSlice("script"))
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	} else if len(scr.Get()) != 0 && version == 1 {
		// check if we are having correct version
		return cli.NewExitError("can not use scripts artifact with version 1", 1)
	}

	err = aw.WriteArtifact("mender", version,
		c.StringSlice("device-type"), c.String("artifact-name"), upd, scr)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	f.Close()
	err = os.Rename(name+".tmp", name)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}
	return nil
}

func read(ar *areader.Reader, verify areader.SignatureVerifyFn,
	readScripts areader.ScriptsReadFn) (*areader.Reader, error) {

	if ar == nil {
		return nil, errors.New("Can not read artifact file.")
	}

	if verify != nil {
		ar.VerifySignatureCallback = verify
	}
	if readScripts != nil {
		ar.ScriptsReadCallback = readScripts
	}

	if err := ar.ReadArtifact(); err != nil {
		return nil, err
	}

	return ar, nil
}

func readArtifact(c *cli.Context) error {
	if c.NArg() == 0 {
		return cli.NewExitError("Nothing specified, nothing read. \nMaybe you wanted"+
			" to say 'artifacts read <pathspec>'?", 1)
	}

	f, err := os.Open(c.Args().First())
	if err != nil {
		return cli.NewExitError("Can not open '"+c.Args().First()+"' file.", 1)
	}
	defer f.Close()

	var verifyCallback areader.SignatureVerifyFn

	if len(c.String("key")) != 0 {
		key, err := getKey(c.String("key"))
		if err != nil {
			return cli.NewExitError(err.Error(), 1)
		}
		s := artifact.NewVerifier(key)
		verifyCallback = s.Verify
	}

	// if key is not provided just continue reading artifact returning
	// info that signature can not be verified
	sigInfo := "no signature"
	ver := func(message, sig []byte) error {
		sigInfo = "signed but no key for verification provided; " +
			"please use `-k` option for providing verification key"
		if verifyCallback != nil {
			err = verifyCallback(message, sig)
			if err != nil {
				sigInfo = "signed; verification using provided key failed"
			} else {
				sigInfo = "signed and verified correctly"
			}
		}
		return nil
	}

	var scripts []string
	readScripts := func(r io.Reader, info os.FileInfo) error {
		scripts = append(scripts, info.Name())
		return nil
	}

	ar := areader.NewReader(f)
	r, err := read(ar, ver, readScripts)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	inst := r.GetHandlers()
	info := r.GetInfo()

	fmt.Printf("Mender artifact:\n")
	fmt.Printf("  Name: %s\n", r.GetArtifactName())
	fmt.Printf("  Format: %s\n", info.Format)
	fmt.Printf("  Version: %d\n", info.Version)
	fmt.Printf("  Signature: %s\n", sigInfo)
	fmt.Printf("  Compatible devices: '%s'\n", r.GetCompatibleDevices())
	if len(scripts) > 0 {
		fmt.Printf("  State scripts:\n")
	}
	for _, scr := range scripts {
		fmt.Printf("    %s\n", scr)
	}
	fmt.Printf("\nUpdates:\n")

	for k, p := range inst {
		fmt.Printf("  %04d:\n", k)
		fmt.Printf("    Type:   %s\n", p.GetType())
		for _, f := range p.GetUpdateFiles() {
			fmt.Printf("    Files:\n")
			fmt.Printf("      name:     %s\n", f.Name)
			fmt.Printf("      size:     %d\n", f.Size)
			fmt.Printf("      modified: %s\n", f.Date)
			fmt.Printf("      checksum: %s\n", f.Checksum)
		}
	}
	return nil
}

func getKey(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Invalid key path: %s", path)
	}
	defer f.Close()

	key := bytes.NewBuffer(nil)
	if _, err := io.Copy(key, f); err != nil {
		return nil, fmt.Errorf("Error reading key: %s", path)
	}
	return key.Bytes(), nil
}

func checkIfValid(artifactPath, keyPath string) error {
	var verifyCallback areader.SignatureVerifyFn

	if keyPath != "" {
		key, err := getKey(keyPath)
		if err != nil {
			return err
		}
		s := artifact.NewVerifier(key)
		verifyCallback = s.Verify
	}

	// do not return error immediately if we can not validate signature;
	// just continue checking consistency and return info if
	// signature verification failed
	valid := true
	ver := func(message, sig []byte) error {
		if verifyCallback != nil {
			if err := verifyCallback(message, sig); err != nil {
				valid = false
			}
		}
		return nil
	}

	f, err := os.Open(artifactPath)
	if err != nil {
		return err
	}
	defer f.Close()

	ar := areader.NewReader(f)
	_, err = read(ar, ver, nil)
	if err != nil {
		return err
	}

	if !valid {
		return errors.New("Artifact file '" + artifactPath +
			"' formatted correctly, but error validating signature.")
	}
	return nil
}

func validateArtifact(c *cli.Context) error {
	if c.NArg() == 0 {
		return cli.NewExitError("Nothing specified, nothing validated. \nMaybe you wanted"+
			" to say 'artifacts validate <pathspec>'?", 1)
	}

	if err := checkIfValid(c.Args().First(), c.String("key")); err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	fmt.Println("Artifact file '" + c.Args().First() + "' validated successfully")
	return nil
}

func signExisting(c *cli.Context) error {
	if c.NArg() == 0 {
		return cli.NewExitError("Nothing specified, nothing signed. \nMaybe you wanted"+
			" to say 'artifacts sign <pathspec>'?", 1)
	}

	if len(c.String("key")) == 0 {
		return cli.NewExitError("Missing signing key; "+
			"please use `-k` parameter for providing one", 1)
	}

	privateKey, err := getKey(c.String("key"))
	if err != nil {
		return cli.NewExitError("Can not use signing key provided: "+err.Error(), 1)
	}

	tFile, err := ioutil.TempFile("", "mender-artifact")
	if err != nil {
		return errors.Wrap(err,
			"Can not create temporary file for storing artifact")
	}
	defer os.Remove(tFile.Name())

	f, err := os.Open(c.Args().First())
	if err != nil {
		return errors.Wrapf(err, "Can not open: %s", c.Args().First())
	}
	defer f.Close()

	reader, err := repack(f, tFile, privateKey, "", "")
	if err != nil {
		return err
	}

	switch ver := reader.GetInfo().Version; ver {
	case 1:
		return cli.NewExitError("Can not sign v1 artifact", 1)
	case 2:
		if reader.IsSigned && !c.Bool("force") {
			return cli.NewExitError("Trying to sign already signed artifact; "+
				"please use force option", 1)
		}
	default:
		return cli.NewExitError("Unsupported version of artifact file: "+string(ver), 1)
	}

	if err = tFile.Close(); err != nil {
		return err
	}

	name := c.Args().First()
	if len(c.String("output-path")) > 0 {
		name = c.String("output-path")
	}

	err = os.Rename(tFile.Name(), name)
	if err != nil {
		os.Remove(tFile.Name())
		return cli.NewExitError("Can not store signed artifact: "+err.Error(), 1)
	}
	return nil
}

func modifyExisting(c *cli.Context, mounted MountPoints) error {
	return nil
}

func unpackArtifact(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", errors.Wrapf(err, "Can not open: %s", name)
	}
	defer f.Close()

	// initialize raw reader and writer
	aReader := areader.NewReader(f)
	rootfs := handlers.NewRootfsInstaller()

	tmp, err := ioutil.TempFile("", "mender-artifact")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	rootfs.InstallHandler = func(r io.Reader, df *handlers.DataFile) error {
		_, err = io.Copy(tmp, r)
		return err
	}

	if err = aReader.RegisterHandler(rootfs); err != nil {
		return "", errors.Wrap(err, "failed to register install handler")
	}

	err = aReader.ReadArtifact()
	if err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

func repack(from io.Reader, to io.Writer, key []byte,
	newName string, dataFile string) (*areader.Reader, error) {
	sDir, err := ioutil.TempDir("", "mender")
	if err != nil {
		return nil, err
	}
	defer os.Remove(sDir)

	storeScripts := func(r io.Reader, info os.FileInfo) error {
		sLocation := filepath.Join(sDir, info.Name())
		f, err := os.OpenFile(sLocation, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return errors.Wrapf(err,
				"can not create script file: %v", sLocation)
		}
		defer f.Close()

		_, err = io.Copy(f, r)
		if err != nil {
			return errors.Wrapf(err,
				"can not write script file: %v", sLocation)
		}
		f.Sync()
		return nil
	}

	verify := func(message, sig []byte) error {
		return nil
	}

	data := dataFile
	ar := areader.NewReader(from)

	if dataFile == "" {
		tmpData, err := ioutil.TempFile("", "mender")
		if err != nil {
			return nil, err
		}
		defer tmpData.Close()

		rootfs := handlers.NewRootfsInstaller()
		rootfs.InstallHandler = func(r io.Reader, df *handlers.DataFile) error {
			_, err := io.Copy(tmpData, r)
			return err
		}
		data = tmpData.Name()
		ar.RegisterHandler(rootfs)
	}

	r, err := read(ar, verify, storeScripts)
	if err != nil {
		return nil, err
	}

	info := r.GetInfo()

	// now once arifact is read we need to
	var h *handlers.Rootfs
	switch info.Version {
	case 1:
		h = handlers.NewRootfsV1(data)
	case 2:
		h = handlers.NewRootfsV2(data)
	default:
		return nil, errors.Errorf("unsupported artifact version: %d", info.Version)
	}

	upd := &awriter.Updates{
		U: []handlers.Composer{h},
	}
	scr, err := scripts([]string{sDir})
	if err != nil {
		return nil, err
	}

	aWriter := awriter.NewWriter(to)
	if key != nil {
		aWriter = awriter.NewWriterSigned(to, artifact.NewSigner(key))
	}

	name := ar.GetArtifactName()
	if newName != "" {
		name = newName
	}
	err = aWriter.WriteArtifact(info.Format, info.Version,
		ar.GetCompatibleDevices(), name, upd, scr)

	return ar, err
}

// oblivious to whether the file exists beforehand
func modifyName(newName, aifp string) error {
	data := []byte(strings.Join([]string{"artifact_name", newName}, "="))
	return ioutil.WriteFile(aifp, data, 0755)
}

func modifyServerCert(newCertPath, certPath string) error {
	newCert, err := os.Open(newCertPath)
	if err != nil {
		return err
	}
	oldCert, err := os.OpenFile(certPath, os.O_RDONLY|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(oldCert, newCert)
	return err
}

func modifyMenderConfVar(confName, newConfVar, mcfp string) error {
	raw, err := ioutil.ReadFile(mcfp)
	if err != nil {
		return err
	}
	var f interface{}
	if err = json.Unmarshal(raw, &f); err != nil {
		return err
	}
	f.(map[string]interface{})[confName] = newConfVar
	data, err := json.Marshal(&f)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(mcfp, data, 0666)
}

func modifyKey(newKeyPath string, mounted MountPoints) error {
	return nil
}

func repackArtifact(artifact, rootfs, key, newName string) error {
	art, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer art.Close()

	tmp, err := ioutil.TempFile("", "mender-artifact")
	if err != nil {
		return err
	}
	defer tmp.Close()

	var privateKey []byte
	if key != "" {
		privateKey, err = getKey(key)
		if err != nil {
			return cli.NewExitError("Can not use signing key provided: "+err.Error(), 1)
		}
	}

	_, err = repack(art, tmp, privateKey, rootfs, newName)
	return err
}

func modifyArtifact(c *cli.Context) error {
	if c.NArg() == 0 {
		return cli.NewExitError("Nothing specified, nothing will be modified. \n"+
			"Maybe you wanted to say 'artifacts read <pathspec>'?", 1)
	}

	fileToModify := c.Args().First()
	isArtifact := false

	// first we need to check  if we are having artifact or image file
	if err := checkIfValid(fileToModify, c.String("key")); err == nil {
		// we have VALID artifact, so we need to unpack it and store header
		isArtifact = true

		fileToModify, err = unpackArtifact(c.Args().First())
		if err != nil {
			return cli.NewExitError("Can not process artifact: "+err.Error(), 1)
		}
	} // else if err == signed but invalid key

	file, err := os.OpenFile(fileToModify, os.O_RDWR, 0)
	if err != nil {
		return cli.NewExitError("Can not open ["+fileToModify+"] file : "+err.Error(), 1)
	}
	defer file.Close()

	// start connection with dbus
	conn, err := dbus.SystemBus()
	if err != nil {
		return cli.NewExitError("Failed to connect to dbus: "+err.Error(), 1)
	}
	defer conn.Close()

	mounted, err := mountFile(conn, file.Fd())
	if err != nil {
		return cli.NewExitError("Can not loop mount file: "+err.Error(), 1)
	}

	if !haveValidDevices(mounted) {
		fmt.Println("Do not have any valid files to modify; exiting")
	} else {
		if err = modifyExisting(c, mounted); err != nil {
			fmt.Printf("Error modifying artifact: %s\n", err.Error())
		}
	}

	err = unmountFile(conn, mounted)
	if err != nil {
		return cli.NewExitError("Can not umount devices: "+err.Error(), 1)
	}

	if isArtifact {
		// re-create the artifact
		err = repackArtifact(c.Args().First(), fileToModify, c.String("key"), c.String("name"))
		if err != nil {
			return cli.NewExitError("Can not recreate artifact: "+err.Error(), 1)
		}
	}
	return nil
}

func run() error {
	app := cli.NewApp()
	app.Name = "mender-artifact"
	app.Usage = "Mender artifact read/writer"
	app.UsageText = "mender-artifact [--version][--help] <command> [<args>]"
	app.Version = Version

	app.Author = "mender.io"
	app.Email = "contact@mender.io"

	//
	// write
	//
	writeRootfs := cli.Command{
		Name: "rootfs-image",
		Action: func(c *cli.Context) error {
			if len(c.StringSlice("device-type")) == 0 ||
				len(c.String("artifact-name")) == 0 ||
				len(c.String("update")) == 0 {
				return cli.NewExitError("must provide `device-type`, `artifact-name` and `update`", 1)
			}
			if len(strings.Fields(c.String("artifact-name"))) > 1 { // check for whitespace in artifact-name
				return cli.NewExitError("whitespace is not allowed in the artifact-name", 1)
			}
			return writeArtifact(c)
		},
	}

	writeRootfs.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "update, u",
			Usage: "Update `FILE`.",
		},
		cli.StringSliceFlag{
			Name: "device-type, t",
			Usage: "Type of device(s) supported by the update. You can specify multiple " +
				"compatible devices providing this parameter multiple times.",
		},
		cli.StringFlag{
			Name:  "artifact-name, n",
			Usage: "Name of the artifact",
		},
		cli.StringFlag{
			Name:  "output-path, o",
			Usage: "Full path to output artifact file.",
		},
		cli.IntFlag{
			Name:  "version, v",
			Usage: "Version of the artifact.",
			Value: LatestFormatVersion,
		},
		cli.StringFlag{
			Name:  "key, k",
			Usage: "Full path to the private key that will be used to sign the artifact.",
		},
		cli.StringSliceFlag{
			Name: "script, s",
			Usage: "Full path to the state script(s). You can specify multiple " +
				"scripts providing this parameter multiple times.",
		},
	}

	write := cli.Command{
		Name:  "write",
		Usage: "Writes artifact file.",
		Subcommands: []cli.Command{
			writeRootfs,
		},
	}

	key := cli.StringFlag{
		Name: "key, k",
		Usage: "Full path to the public key that will be used to verify " +
			"the artifact signature.",
	}

	//
	// validate
	//
	validate := cli.Command{
		Name:        "validate",
		Usage:       "Validates artifact file.",
		Action:      validateArtifact,
		UsageText:   "mender-artifact validate [options] <pathspec>",
		Description: "This command validates artifact file provided by pathspec.",
	}
	validate.Flags = []cli.Flag{
		key,
	}

	//
	// read
	//
	read := cli.Command{
		Name:        "read",
		Usage:       "Reads artifact file.",
		Action:      readArtifact,
		UsageText:   "mender-artifact read [options] <pathspec>",
		Description: "This command validates artifact file provided by pathspec.",
	}

	read.Flags = []cli.Flag{
		key,
	}

	//
	// sign
	//
	sign := cli.Command{

		Name:        "sign",
		Usage:       "Signs existing artifact file.",
		Action:      signExisting,
		UsageText:   "mender-artifact sign [options] <pathspec>",
		Description: "This command signs artifact file provided by pathspec.",
	}
	sign.Flags = []cli.Flag{
		key,
		cli.StringFlag{
			Name: "output-path, o",
			Usage: "Full path to output signed artifact file; " +
				"if none is provided existing artifact will be replaced with signed one",
		},
		cli.BoolFlag{
			Name:  "force, f",
			Usage: "Force creating new signature if the artifact is already signed",
		},
	}

	//
	// modify existing
	//
	modify := cli.Command{
		Name:        "modify",
		Usage:       "Modifies image or artifact file.",
		Action:      modifyArtifact,
		UsageText:   "mender-artifact modify [options] <pathspec>",
		Description: "This command modifies existing image or artifact file provided by pathspec.",
	}

	modify.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "key, k",
			Usage: "Full path to the private key that will be used to sign the artifact after modifying.",
		},
		cli.StringFlag{
			Name:  "server-uri, u",
			Usage: "Mender server URI; the default URI will be replaced with given one.",
		},
		cli.StringFlag{
			Name: "server-cert, c",
			Usage: "Full path to the certificate file that will be used for validating " +
				"Mender server by the client.",
		},
		cli.StringFlag{
			Name: "verification-key, v",
			Usage: "Full path to the public verification key that is used by the client  " +
				"to verify the artifact.",
		},
		cli.StringFlag{
			Name:  "name, n",
			Usage: "New name of the artifact.",
		},
		cli.StringFlag{
			Name:  "tenant-token, t",
			Usage: "Full path to the tenant token that will be injected into modified file.",
		},
	}

	app.Commands = []cli.Command{
		write,
		read,
		validate,
		sign,
		modify,
	}
	return app.Run(os.Args)
}

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}
