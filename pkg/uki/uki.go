// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package uki creates the UKI file out of the sd-stub and other sections.
package uki

import (
	"errors"
	"fmt"
	"github.com/kairos-io/go-ukify/pkg/types"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/kairos-io/go-ukify/pkg/pesign"
)

// Builder is a UKI file builder.
type Builder struct {
	// Source options.
	//
	// Arch of the UKI file.
	Arch string
	// Version of Talos.
	Version string
	// Path to the sd-stub.
	SdStubPath string
	// Path to the sd-boot.
	SdBootPath string
	// Path to the kernel image.
	KernelPath string
	// Path to the initrd image.
	InitrdPath string
	// Kernel cmdline.
	Cmdline string
	// Os-release file
	OsRelease string
	// SecureBoot certificate and signer.
	SecureBootSigner pesign.CertificateSigner
	// SecureBoot key
	SBKey string
	// SecureBoot cert
	SBCert string

	// PCR signer.
	PCRSigner types.RSAKey
	// Path to the PCR signing key
	PCRKey string

	Splash string

	// Output options:
	//
	// Path to the signed sd-boot.
	OutSdBootPath string
	// Path to the output UKI file.
	OutUKIPath string

	// Logger
	Logger   *slog.Logger
	LogLevel string

	// fields initialized during build
	sections        []types.UkiSection
	scratchDir      string
	peSigner        *pesign.Signer
	unsignedUKIPath string
}

// Build the UKI file.
//
// Build process is as follows:
//   - sign the sd-boot EFI binary, and write it to the OutSdBootPath
//   - build ephemeral sections (uname, os-release), and other proposed sections
//   - measure sections, generate signature, and append to the list of sections
//   - assemble the final UKI file starting from sd-stub and appending generated section.
func (builder *Builder) Build() error {
	var err error

	if builder.Logger == nil {
		builder.Logger = slog.Default()
	}
	switch strings.ToLower(builder.LogLevel) {
	case "debug":
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case "warn":
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case "error":
		slog.SetLogLoggerLevel(slog.LevelError)
	default:
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}

	if builder.PCRSigner == nil {
		if builder.PCRKey == "" {
			return errors.New("no PCR signer or PCRKey available")
		} else {
			signer, err := pesign.NewPCRSigner(builder.PCRKey)
			if err != nil {
				return err
			}
			builder.PCRSigner = signer
		}
	}

	if builder.SecureBootSigner == nil {
		if builder.SBCert == "" || builder.SBKey == "" {
			return errors.New("no Secureboot signer or combination of SB key+cert to sign")
		} else {
			sbSigner, err := pesign.NewSecureBootSigner(builder.SBCert, builder.SBKey)
			if err != nil {
				return err
			}
			builder.SecureBootSigner = sbSigner
		}
	}

	builder.scratchDir, err = os.MkdirTemp("", "ukify")
	if err != nil {
		return err
	}

	defer func() {
		if err = os.RemoveAll(builder.scratchDir); err != nil {
			log.Printf("failed to remove scratch dir: %v", err)
		}
	}()

	if builder.SdBootPath != "" {
		slog.Info("Signing systemd-boot", "path", builder.SdBootPath)

		builder.peSigner, err = pesign.NewSigner(builder.SecureBootSigner)
		if err != nil {
			return fmt.Errorf("error initializing signer: %w", err)
		}

		// sign sd-boot
		if err = builder.peSigner.Sign(builder.SdBootPath, builder.OutSdBootPath, builder.Logger); err != nil {
			return fmt.Errorf("error signing sd-boot: %w", err)
		}
		slog.Info("Signed systemd-boot", "path", builder.OutSdBootPath)
	} else {
		builder.Logger.Info("Not signing systemd-boot")
	}

	builder.Logger.Info("Generating UKI sections")

	// generate and build list of all sections
	for _, generateSection := range []func() error{
		builder.generateOSRel,
		builder.generateCmdline,
		builder.generateInitrd,
		builder.generateSplash,
		builder.generateUname,
		builder.generateSBAT,
		builder.generatePCRPublicKey,
		// append kernel last to account for decompression
		builder.generateKernel,
		// measure sections last
		builder.generatePCRSig,
	} {
		if err = generateSection(); err != nil {
			return fmt.Errorf("error generating sections: %w", err)
		}
	}

	builder.Logger.Info("Generated UKI sections")

	builder.Logger.Info("Assembling UKI")

	// assemble the final UKI file
	if err = builder.assemble(); err != nil {
		return fmt.Errorf("error assembling UKI: %w", err)
	}

	builder.Logger.Info("Assembled UKI")
	builder.Logger.Info("Signing UKI")

	// sign the UKI file
	err = builder.peSigner.Sign(builder.unsignedUKIPath, builder.OutUKIPath, builder.Logger)
	if err == nil {
		builder.Logger.Info("Signed UKI")
	}
	return err
}
