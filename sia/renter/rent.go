package renter

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/NebulousLabs/Sia/consensus"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/hash"
	"github.com/NebulousLabs/Sia/sia/components"
)

// ClientFundFileContract takes a template FileContract and returns a
// partial transaction containing an input for the contract, but no signatures.
//
// TODO: We need to get the id of the contract before we can start doing
// re-uploading.
func (r *Renter) proposeContract(filename string, duration consensus.BlockHeight) (fp FilePiece, err error) {
	// Open the file, create a merkle hash.
	file, err := os.Open(filename)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return
	}
	merkle, err := hash.ReaderMerkleRoot(file, hash.CalculateSegments(uint64(info.Size())))
	if err != nil {
		return
	}
	// reset read position
	if _, err = file.Seek(0, 0); err != nil {
		return
	}

	// Find a host. If the search or the negotiation is unsuccessful,
	// hostdb.FlagHost() will be called and another host will be requested. If
	// there is an internal error (no hosts, or an unsuccessful flagging for
	// example), the loop will break.
	var host components.HostEntry
	var fileContract consensus.FileContract
	for {
		host, err = r.hostDB.RandomHost()
		if err != nil {
			return
		}

		// Fill out the contract according to the whims of the host.
		// The contract fund: (burn * duration + price * full duration) * filesize
		delay := consensus.BlockHeight(20)
		contractFund := (host.Price*consensus.Currency(duration+delay) + host.Burn*consensus.Currency(duration)) * consensus.Currency(info.Size())
		fileContract = consensus.FileContract{
			ContractFund:       contractFund,
			FileMerkleRoot:     merkle,
			FileSize:           uint64(info.Size()),
			Start:              r.state.Height() + delay,
			End:                r.state.Height() + duration + delay,
			ChallengeWindow:    host.Window,
			Tolerance:          host.Tolerance,
			ValidProofPayout:   host.Price * consensus.Currency(info.Size()) * consensus.Currency(host.Window),
			ValidProofAddress:  host.CoinAddress,
			MissedProofPayout:  host.Burn * consensus.Currency(info.Size()) * consensus.Currency(host.Window),
			MissedProofAddress: consensus.CoinAddress{}, // The empty address is the burn address.
		}

		// Fund the client portion of the transaction.
		minerFee := consensus.Currency(10) // TODO: ask wallet.
		renterPortion := host.Price * consensus.Currency(duration) * consensus.Currency(fileContract.FileSize)
		var id string
		id, err = r.wallet.RegisterTransaction(consensus.Transaction{})
		if err != nil {
			return
		}
		err = r.wallet.FundTransaction(id, renterPortion+minerFee)
		if err != nil {
			return
		}
		err = r.wallet.AddMinerFee(id, minerFee)
		if err != nil {
			return
		}
		err = r.wallet.AddFileContract(id, fileContract)
		if err != nil {
			return
		}
		var transaction consensus.Transaction
		transaction, err = r.wallet.SignTransaction(id, false)
		if err != nil {
			return
		}

		// Negotiate the contract to the host.
		err = host.IPAddress.Call("NegotiateContract", func(conn net.Conn) error {
			// send contract
			if _, err := encoding.WriteObject(conn, transaction); err != nil {
				return err
			}
			// read response
			var response string
			if err := encoding.ReadObject(conn, &response, 128); err != nil {
				return err
			}
			if response != components.AcceptContractResponse {
				return errors.New(response)
			}
			// host accepted, so transmit file data
			// (no prefix needed, since FileSize is included in the metadata)
			_, err = io.CopyN(conn, file, info.Size())
			return err
		})
		if err == nil {
			break
		}

		fmt.Println("Problem from NegotiateContract:", err)
		err = r.hostDB.FlagHost(host.ID)
		if err != nil {
			return
		}
	}

	// Record the file in to the renter database.
	fp = FilePiece{
		Host:     host,
		Contract: fileContract,
	}

	return
}

// TODO: Do the uploading in parallel.
func (r *Renter) RentFile(rfp components.RentFileParameters) (err error) {
	r.lock()
	defer r.unlock()

	_, exists := r.files[rfp.Nickname]
	if exists {
		return errors.New("file of that nickname already exists")
	}

	// Make an entry for this file.
	var pieces []FilePiece
	for i := 0; i < rfp.TotalPieces; i++ {
		var piece FilePiece
		piece, err = r.proposeContract(rfp.Filepath, consensus.BlockHeight(2000+1000*i))
		if err != nil {
			return
		}
		pieces = append(pieces, piece)
	}

	r.files[rfp.Nickname] = FileEntry{Pieces: pieces}
	return
}
