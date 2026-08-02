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

//go:generate go run ../cmd/tools/decorate.go -f decorateCompleoSolo -b *CompleoSolo -r api.Charger -t "api.Meter,CurrentPower,func() (float64, error)" -t "api.PhaseCurrents,Currents,func() (float64, float64, float64, error)" -t "api.PhaseVoltages,Voltages,func() (float64, float64, float64, error)" -t "api.ChargeRater,ChargedEnergy,func() (float64, error)" -t "api.MeterEnergy,TotalEnergy,func() (float64, error)" -t "api.Identifier,Identify,func() (string, error)"

// NewCompleoSoloAdv creates CompleoSolo charger
func NewCompleoSoloAdv(ctx context.Context, uri string, id uint8) (api.Charger, error) {
	fmt.Printf("uri: %s\n", uri)

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
		_timeout:  30,
	}

	// timeout 30 Sekunden
	wb.conn.WriteSingleRegister(compleoAdvRegTimeout, wb._timeout)
	// configure to 11kW
	wb.conn.WriteSingleRegister(compleoAdvRegMaxPower, wb._maxPower)
	wb.conn.WriteSingleRegister(compleoAdvRegMaxPowerFallback, wb._maxPower)
	// 100%
	wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentage, 100)
	wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentageFallback, 0) // OFF without control!
	// 20 A unbalanced
	wb.conn.WriteSingleRegister(compleoAdvRegMaxUnbalancedLoad, 2000)

	return wb, err
}

// Status implements the api.Charger interface
func (wb *CompleoSoloAdv) Status() (api.ChargeStatus, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPStatus, 1)
	if err != nil {
		return api.StatusNone, err
	}

	s := binary.BigEndian.Uint16(b)
	wb.log.TRACE.Printf("compleoAdvRegCPStatus: %d\n", s)

	switch s {
	case 0:
		return api.StatusA, nil
	case 3:
		return api.StatusC, nil
	case 5, 7:
		return api.StatusB, nil
	default:
		return api.StatusNone, fmt.Errorf("invalid status: %d\n", s)
	}
}

// Enabled implements the api.Charger interface
func (wb *CompleoSoloAdv) Enabled() (bool, error) {
	//	fmt.Print("Enabled")

	// this is NOT in the documentation - there Holding-type is documented
	b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegPowerPercentage, 2)
	if err != nil {

		return false, fmt.Errorf("Enabled Error Writing reg err=%s\n", err.Error())
	}
	var percent uint16 = binary.BigEndian.Uint16(b)
	wb.log.TRACE.Printf("Enabled read: %d %d %d", percent, b[0], b[1])
	return percent != 0, nil
}

// Enable implements the api.Charger interface
func (wb *CompleoSoloAdv) Enable(enable bool) error {
	wb.log.TRACE.Printf("Enable: %t\n", enable)

	var percent uint16
	if enable {
		percent = 100
	} else {
		percent = 0
	}

	_, err := wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentage, percent)

	if err != nil {
		return fmt.Errorf("Error Writing compleoAdvRegPowerPercentage: %d\n", percent)
	}

	return err
}

// MaxCurrent implements the api.Charger interface
func (wb *CompleoSoloAdv) MaxCurrent(current int64) error {
	wb.log.TRACE.Printf("MaxCurrent: %d\n", current)

	//	var percent uint16
	//	percent = uint16(float64(current) * 100 / 16)
	//	_, err := wb.conn.WriteSingleRegister(compleoAdvRegPowerPercentage, percent)
	var regval uint16
	regval = uint16(float64(current)*110.0/16.0) + 1
	regval = uint16(math.Min(110, float64(regval)))
	_, err := wb.conn.WriteSingleRegister(compleoAdvRegCPMaxPower, regval)

	if err != nil {
		return fmt.Errorf("MaxCurrent Write Error err=%s\n", err.Error())
	} else {
		wb.log.TRACE.Printf("Wrote reg compleoAdvRegCPMaxPower : %d\n", regval)
	}

	return nil
}

var _ api.ChargeTimer = (*CompleoSoloAdv)(nil)

// ChargingTime implements the api.ChargeTimer interface
func (wb *CompleoSoloAdv) ChargeDuration() (time.Duration, error) {
	wb.log.TRACE.Println("ChargingTime")
	var sec uint32

	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPChargingTime0, 2)
	if err != nil {
		return 0, err
	}
	sec = uint32(binary.BigEndian.Uint16(b))
	b, err = wb.conn.ReadInputRegisters(compleoAdvRegCPChargingTime1, 2)
	if err != nil {
		return 0, err
	}
	sec = sec + (uint32(binary.BigEndian.Uint16(b)) << 16)

	wb.log.DEBUG.Printf("ChargingTime: %d seconds\n", sec)
	return time.Duration(sec) * time.Second, nil
}

var _ api.Meter = (*CompleoSoloAdv)(nil)

// CurrentPower implements the api.Meter interface
func (wb *CompleoSoloAdv) CurrentPower() (float64, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPPower, 2)
	if err != nil {
		return 0, fmt.Errorf("Error Reading compleoAdvRegCPPower err=%s\n", err.Error())
	}
	var val uint16
	val = binary.BigEndian.Uint16(b)
	val *= 100

	wb.log.TRACE.Printf("Reading compleoAdvRegCPPower: %d\n", val)
	return float64(val), nil
}

// ChargedEnergy implements the api.ChargeRater interface
func (wb *CompleoSoloAdv) chargedEnergy() (float64, error) {
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPChargedEnergy, 2)
	if err != nil {
		return 0, err
	}

	return float64(binary.BigEndian.Uint16(b)) * 100, nil
}

// TotalEnergy implements the api.MeterEnergy interface
func (wb *CompleoSoloAdv) totalEnergy() (float64, error) {

	return wb.chargedEnergy()
}

// currents implements the api.PhaseCurrents interface
func (wb *CompleoSoloAdv) currents() (float64, float64, float64, error) {
	var curr [3]float64
	b, err := wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase1, 2)
	if err != nil {
		return 0, 0, 0, err
	}
	curr[0] = float64(binary.BigEndian.Uint16(b)) * 0.1

	b, err = wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase2, 2)
	if err != nil {
		return 0, 0, 0, err
	}
	curr[1] = float64(binary.BigEndian.Uint16(b)) * 0.1

	b, err = wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase3, 2)
	if err != nil {
		return 0, 0, 0, err
	}
	curr[2] = float64(binary.BigEndian.Uint16(b)) * 0.1

	return curr[0], curr[1], curr[2], nil
}

// voltages implements the api.PhaseVoltages interface
func (wb *CompleoSoloAdv) voltages() (float64, float64, float64, error) {

	return 230.0, 230.0, 230.0, nil
}

// identify implements the api.Identifier interface
func (wb *CompleoSoloAdv) identify() (string, error) {

	return "CompleoID", nil
}

var _ api.Diagnosis = (*CompleoSoloAdv)(nil)

// Diagnose implements the api.Diagnosis interface
func (wb *CompleoSoloAdv) Diagnose() {
	fmt.Printf("\tModel:\tCompleo Solo SAM Advanced\n")

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegNumberOfChargePoints, 2); err == nil {
		fmt.Printf("\tChargepoints:\t%d\n", binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegFirmwareVersion0, 4); err == nil {
		fmt.Printf("\tFirmware:\t%d.%d.%d\n", b[2], b[3], b[0])
	}

	if b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegMaxPower, 2); err == nil {
		fmt.Printf("\tMax Power:\t%d W\n", 100*binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegPowerPercentage, 2); err == nil {
		fmt.Printf("\tMax Powerpercentage:\t%d %%\n", binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegMaxUnbalancedLoad, 2); err == nil {
		fmt.Printf("\tMax MaxUnbalanced:%d \t%f \n", binary.BigEndian.Uint16(b), float64(binary.BigEndian.Uint16(b))*0.1)
	}

	if b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegMaxPowerFallback, 2); err == nil {
		fmt.Printf("\tMax PowerFB:\t%d W\n", 100*binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadHoldingRegisters(compleoAdvRegPowerPercentageFallback, 2); err == nil {
		fmt.Printf("\tMax PowerpercentageFB:\t%d %% \n", binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegPower, 2); err == nil {
		fmt.Printf("\tCurr Power:\t%d W\n", 100*binary.BigEndian.Uint16(b))
	}
	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase1, 2); err == nil {
		fmt.Printf("\tAct. Current P1:\t%f A\n", 0.1*float64(binary.BigEndian.Uint16(b)))
	}
	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase2, 2); err == nil {
		fmt.Printf("\tAct. Current P2:\t%f A\n", 0.1*float64(binary.BigEndian.Uint16(b)))
	}
	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegCurrentPhase3, 2); err == nil {
		fmt.Printf("\tAct. Current P3:\t%f A\n", 0.1*float64(binary.BigEndian.Uint16(b)))
	}

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegUnusedPower, 2); err == nil {
		fmt.Printf("\tUnused Power:\t%d W\n", 100*binary.BigEndian.Uint16(b))
	}

	if b, err := wb.conn.ReadInputRegisters(compleoAdvRegCPStatus, 2); err == nil {
		fmt.Printf("\tcompleoAdvRegCPStatus:\t%d W\n", binary.BigEndian.Uint16(b))
	}
}
