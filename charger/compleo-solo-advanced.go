package charger

// LICENSE

// Copyright (c) 2022 premultiply, MrtT500

// This module is NOT covered by the MIT license. All rights reserved.

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Supports all chargers based on Bender CC612/613 controller series
// * The 'Modbus TCP Server for energy management systems' must be enabled.
// * The setting 'Register Address Set' must NOT be set to 'Phoenix', 'TQ-DM100' or 'ISE/IGT Kassel'.
//   -> Use the third selection labeled 'Ebee', 'Bender', 'MENNEKES' etc.
// * Set 'Allow UID Disclose' to On

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
)

// CompleoSoloAdv charger implementation
type CompleoSoloAdv struct {
	log       *util.Logger
	conn      *modbus.Connection
	_maxPower uint16
	_timeout  uint16
}

const (
	// all holding regs - read/write
	compleoAdvRegMaxPower                = 0
	compleoAdvRegPowerPercentage         = 1
	compleoAdvRegMaxUnbalancedLoad       = 2
	compleoAdvRegMaxPowerFallback        = 3
	compleoAdvRegPowerPercentageFallback = 4
	compleoAdvRegTimeout                 = 5

	// input registers - read-only
	compleoAdvRegFirmwareVersion0     = 6
	compleoAdvRegFirmwareVersion1     = 7
	compleoAdvRegNumberOfChargePoints = 8
	compleoAdvRegPower                = 9
	compleoAdvRegCurrentPhase1        = 10
	compleoAdvRegCurrentPhase2        = 11
	compleoAdvRegCurrentPhase3        = 12
	compleoAdvRegUnusedPower          = 13

	// all these registers are per charge point
	// base address 0x0100 + charge point # * 0x0010
	// 256 + charge point # * 16
	compleoAdvRegCPMaxPower      = 256 // rw
	compleoAdvRegCPStatus        = 257 // r, bit 0=active, 1=charging, 2=limited, 3=error, 4=prefcp1, 5=prefcp2
	compleoAdvRegCPPower         = 258 // r
	compleoAdvRegCPCurrentPhase1 = 259 // r
	compleoAdvRegCPCurrentPhase2 = 260 // r
	compleoAdvRegCPCurrentPhase3 = 261 // r
	compleoAdvRegCPChargingTime0 = 262 // r
	compleoAdvRegCPChargingTime1 = 263 // r
	compleoAdvRegCPChargedEnergy = 264 // r
)

func init() {
	registry.AddCtx("compleo-solo-adv", NewCompleoSoloAdvFromConfig)
}

// NewCompleoSoloAdvFromConfig creates a CompleoSolo charger from generic config
func NewCompleoSoloAdvFromConfig(ctx context.Context, other map[string]interface{}) (api.Charger, error) {
	cc := modbus.TcpSettings{
		ID: 255,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	return NewCompleoSoloAdv(ctx, cc.URI, cc.ID)
}

// NewCompleoSoloAdv creates CompleoSolo charger
func NewCompleoSoloAdv(ctx context.Context, uri string, id uint8) (api.Charger, error) {
	conn, err := modbus.NewConnection(ctx, uri, "", "", 0, modbus.Tcp, id)
	if err != nil {
		return nil, err
	}

	log := util.NewLogger("compleo solo")
	conn.Logger(log.TRACE)

	wb := &CompleoSoloAdv{
		log:       log,
		conn:      conn,
		_maxPower: 110,
		// 120s überbrückt die periodischen Netzwerkausfälle der Box (~45s),
		// stoppt die Ladung aber weiterhin, falls evcc komplett ausfällt
		_timeout: 120,
	}

	wb.conn.WriteSingleRegister(compleoAdvRegTimeout, wb._timeout)
	// configure to 11kW
	wb.conn.WriteSingleRegister(compleoAdvRegMaxPower, wb._maxPower)
	wb.conn.WriteSingleRegister(compleoAdvRegMaxPowerFallback, wb._maxPower)
	// 100%
	wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentage, 100)
	wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentageFallback, 0) // OFF without control!
	// Schieflast verwaltet die Box selbst (11-kW-Box: max. 16,0 A = Registerwert 160);
	// der frühere Write von 2000 wurde von der Box verworfen

	// heartbeat am tatsächlich in der Box aktiven Timeout ausrichten,
	// falls der Schreibzugriff oben nicht angekommen ist
	b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegTimeout, 1)
	if err != nil {
		return nil, fmt.Errorf("failsafe timeout: %w", err)
	}
	if u := binary.BigEndian.Uint16(b); u > 0 {
		go wb.heartbeat(ctx, time.Duration(u)*time.Second/2)
	}

	return wb, err
}

// heartbeat hält den Kommunikations-Watchdog der Box unabhängig vom
// evcc-Poll-Intervall am Leben; jede Modbus-Transaktion setzt ihn zurück
func (wb *CompleoSoloAdv) heartbeat(ctx context.Context, timeout time.Duration) {
	for tick := time.Tick(timeout); ; {
		select {
		case <-tick:
		case <-ctx.Done():
			return
		}

		if _, err := wb.conn.ReadInputRegisters(compleoAdvRegCPStatus, 1); err != nil {
			wb.log.ERROR.Println("heartbeat:", err)
		}
	}
}

// Status implements the api.Charger interface
func (wb *CompleoSoloAdv) Status() (api.ChargeStatus, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPStatus, 1)
	if err != nil {
		return api.StatusNone, err
	}

	s := binary.BigEndian.Uint16(b)
	wb.log.TRACE.Printf("compleoAdvRegCPStatus: %d", s)

	// Bitfeld: Bit 0=Aktiv, 1=Lädt, 2=Begrenzt, 3=Fehler, 4/5=Bevorzugter Ladepunkt
	if s&8 != 0 {
		return api.StatusNone, fmt.Errorf("status error bit set: %d", s)
	}

	res := api.StatusA
	if s&1 != 0 {
		res = api.StatusB
	}
	if s&2 != 0 {
		res = api.StatusC
	}

	return res, nil
}

// Enabled implements the api.Charger interface
func (wb *CompleoSoloAdv) Enabled() (bool, error) {
	// die Box rechnet den Prozentwert selbst um (z.B. 100 -> 99),
	// daher nur auf 0 / nicht 0 prüfen
	b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegPowerPercentage, 1)
	if err != nil {
		return false, err
	}

	return binary.BigEndian.Uint16(b) != 0, nil
}

// Enable implements the api.Charger interface
func (wb *CompleoSoloAdv) Enable(enable bool) error {
	wb.log.TRACE.Printf("Enable: %t", enable)

	var percent uint16
	if enable {
		percent = 100
	}

	_, err := wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentage, percent)
	return err
}

// MaxCurrent implements the api.Charger interface
func (wb *CompleoSoloAdv) MaxCurrent(current int64) error {
	regval := uint16(float64(current)*110.0/16.0) + 1
	regval = uint16(math.Min(110, float64(regval)))

	wb.log.TRACE.Printf("MaxCurrent %d A -> compleoAdvRegCPMaxPower %d", current, regval)

	_, err := wb.conn.WriteSingleRegister(compleoAdvRegCPMaxPower, regval)
	return err
}

var _ api.ChargeTimer = (*CompleoSoloAdv)(nil)

// ChargingTime implements the api.ChargeTimer interface
func (wb *CompleoSoloAdv) ChargeDuration() (time.Duration, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPChargingTime0, 2)
	if err != nil {
		return 0, err
	}

	// Sekunden, LSW first
	sec := uint32(binary.BigEndian.Uint16(b)) | uint32(binary.BigEndian.Uint16(b[2:]))<<16

	wb.log.DEBUG.Printf("ChargingTime: %d seconds", sec)
	return time.Duration(sec) * time.Second, nil
}

var _ api.Meter = (*CompleoSoloAdv)(nil)

// CurrentPower implements the api.Meter interface
func (wb *CompleoSoloAdv) CurrentPower() (float64, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPPower, 1)
	if err != nil {
		return 0, err
	}

	return float64(binary.BigEndian.Uint16(b)) * 100, nil
}

var _ api.ChargeRater = (*CompleoSoloAdv)(nil)

// ChargedEnergy implements the api.ChargeRater interface
func (wb *CompleoSoloAdv) ChargedEnergy() (float64, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPChargedEnergy, 1)
	if err != nil {
		return 0, err
	}

	// 100Wh-Schritte -> kWh
	return float64(binary.BigEndian.Uint16(b)) / 10, nil
}

var _ api.PhaseCurrents = (*CompleoSoloAdv)(nil)

// Currents implements the api.PhaseCurrents interface
func (wb *CompleoSoloAdv) Currents() (float64, float64, float64, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase1, 3)
	if err != nil {
		return 0, 0, 0, err
	}

	var curr [3]float64
	for i := range curr {
		curr[i] = float64(binary.BigEndian.Uint16(b[2*i:])) / 10
	}

	return curr[0], curr[1], curr[2], nil
}

var _ api.Diagnosis = (*CompleoSoloAdv)(nil)

// diagnoseReg reads a single register and prints value or error
func (wb *CompleoSoloAdv) diagnoseReg(label string, holding bool, addr uint16, format func(uint16) string) {
	var b []byte
	var err error
	if holding {
		b, err = wb.conn.ReadHoldingRegisters(addr, 1)
	} else {
		b, err = wb.conn.ReadInputRegisters(addr, 1)
	}

	if err != nil {
		fmt.Printf("\t%s:\terror: %v\n", label, err)
		return
	}

	fmt.Printf("\t%s:\t%s\n", label, format(binary.BigEndian.Uint16(b)))
}

// Diagnose implements the api.Diagnosis interface
func (wb *CompleoSoloAdv) Diagnose() {
	watt := func(v uint16) string { return fmt.Sprintf("%d W", 100*uint32(v)) }
	percent := func(v uint16) string { return fmt.Sprintf("%d %%", v) }
	ampere := func(v uint16) string { return fmt.Sprintf("%.1f A", float64(v)/10) }
	plain := func(v uint16) string { return fmt.Sprintf("%d", v) }

	fmt.Printf("\tModel:\tCompleo Solo SAM Advanced\n")

	wb.diagnoseReg("Chargepoints", false, compleoAdvRegNumberOfChargePoints, plain)

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegFirmwareVersion0, 2); err == nil {
		fmt.Printf("\tFirmware:\t%d.%d.%d\n", b[2], b[3], b[0])
	} else {
		fmt.Printf("\tFirmware:\terror: %v\n", err)
	}

	wb.diagnoseReg("Max Power", true, compleoAdvRegMaxPower, watt)
	wb.diagnoseReg("Power Percentage", true, compleoAdvRegPowerPercentage, percent)
	wb.diagnoseReg("Max Unbalanced Load", true, compleoAdvRegMaxUnbalancedLoad, ampere)
	wb.diagnoseReg("Max Power Fallback", true, compleoAdvRegMaxPowerFallback, watt)
	wb.diagnoseReg("Power Percentage Fallback", true, compleoAdvRegPowerPercentageFallback, percent)
	wb.diagnoseReg("Timeout", true, compleoAdvRegTimeout, func(v uint16) string {
		return fmt.Sprintf("%d s (Fallback nach %d s)", v, 2*uint32(v))
	})
	wb.diagnoseReg("Current Power", false, compleoAdvRegPower, watt)
	wb.diagnoseReg("Current P1", false, compleoAdvRegCurrentPhase1, ampere)
	wb.diagnoseReg("Current P2", false, compleoAdvRegCurrentPhase2, ampere)
	wb.diagnoseReg("Current P3", false, compleoAdvRegCurrentPhase3, ampere)
	wb.diagnoseReg("Unused Power", false, compleoAdvRegUnusedPower, watt)
	wb.diagnoseReg("CP Max Power", true, compleoAdvRegCPMaxPower, watt)
	wb.diagnoseReg("CP Status", false, compleoAdvRegCPStatus, func(v uint16) string {
		return fmt.Sprintf("%d (Bits: %04b)", v, v)
	})
	wb.diagnoseReg("CP Charged Energy", false, compleoAdvRegCPChargedEnergy, func(v uint16) string {
		return fmt.Sprintf("%.1f kWh", float64(v)/10)
	})
}
