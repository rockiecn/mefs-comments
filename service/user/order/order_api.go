package order

import (
	"context"
	"math/big"

	"github.com/memoio/go-mefs-v2/api"
	"github.com/memoio/go-mefs-v2/lib/pb"
	"github.com/memoio/go-mefs-v2/lib/types"
	"github.com/memoio/go-mefs-v2/lib/types/store"

	"golang.org/x/xerrors"
)

func (m *OrderMgr) OrderList(_ context.Context) ([]uint64, error) {
	res := make([]uint64, 0, len(m.pros))
	res = append(res, m.pros...)
	return res, nil
}

func (m *OrderMgr) OrderGetJobInfo(ctx context.Context) ([]*api.OrderJobInfo, error) {
	res := make([]*api.OrderJobInfo, 0, len(m.pros))

	for _, pid := range m.pros {
		oi, err := m.OrderGetJobInfoAt(ctx, pid)
		if err != nil {
			continue
		}

		res = append(res, oi)
	}

	return res, nil
}

func (m *OrderMgr) OrderGetJobInfoAt(_ context.Context, proID uint64) (*api.OrderJobInfo, error) {
	m.lk.RLock()
	defer m.lk.RUnlock()
	of, ok := m.orders[proID]
	if ok {
		oi := &api.OrderJobInfo{
			ID: proID,

			AvailTime:  of.availTime,
			Nonce:      of.nonce,
			OrderTime:  of.orderTime,
			OrderState: string(of.orderState),

			SeqNum:   of.seqNum,
			SeqTime:  of.seqTime,
			SeqState: string(of.seqState),

			Jobs: of.segCount(),

			Ready:  of.ready,
			InStop: of.inStop,
		}

		if of.base != nil {
			oi.Nonce = of.base.Nonce
		}

		if of.seq != nil {
			oi.SeqNum = of.seq.SeqNum
		}

		pid, err := m.GetPeerIDAt(m.ctx, proID)
		if err == nil {
			oi.PeerID = pid.Pretty()
		}

		return oi, nil
	}

	return nil, xerrors.Errorf("not found")
}

func (m *OrderMgr) OrderGetDetail(ctx context.Context, proID, nonce uint64, seqNum uint32) (*types.SignedOrderSeq, error) {

	key := store.NewKey(pb.MetaType_OrderSeqKey, m.localID, proID, nonce, seqNum)
	val, err := m.ds.Get(key)
	if err != nil {
		return nil, err
	}

	sos := new(types.SignedOrderSeq)
	err = sos.Deserialize(val)
	if err != nil {
		return nil, err
	}

	return sos, nil
}

func (m *OrderMgr) OrderGetPayInfoAt(ctx context.Context, pid uint64) (*types.OrderPayInfo, error) {
	m.sizelk.RLock()
	defer m.sizelk.RUnlock()
	pi := new(types.OrderPayInfo)
	if pid == 0 {
		pi.Size = m.opi.Size // may be less than
		pi.ConfirmSize = m.opi.ConfirmSize
		pi.OnChainSize = m.opi.OnChainSize
		pi.NeedPay = new(big.Int).Set(m.opi.NeedPay)
		pi.Paid = new(big.Int).Set(m.opi.Paid)

		bi, err := m.is.SettleGetBalanceInfo(ctx, m.localID)
		if err != nil {
			return nil, err
		}
		pi.Balance = new(big.Int).Set(bi.ErcValue)
		pi.Balance.Add(pi.Balance, bi.FsValue)
	} else {
		m.lk.RLock()
		of, ok := m.orders[pid]
		m.lk.RUnlock()
		if ok {
			pi.ID = pid
			pi.Size = of.opi.Size
			pi.ConfirmSize = of.opi.ConfirmSize
			pi.OnChainSize = of.opi.OnChainSize
			pi.NeedPay = new(big.Int).Set(of.opi.NeedPay)
			pi.Paid = new(big.Int).Set(of.opi.Paid)

			if of.seq != nil {
				pi.Size += of.seq.Size
			} else {
				if of.base != nil {
					pi.Size += of.base.Size
				}
			}
		}
	}

	return pi, nil
}

func (m *OrderMgr) OrderGetPayInfo(ctx context.Context) ([]*types.OrderPayInfo, error) {
	res := make([]*types.OrderPayInfo, 0, len(m.pros))

	for _, pid := range m.pros {
		oi, err := m.OrderGetPayInfoAt(ctx, pid)
		if err != nil {
			continue
		}

		res = append(res, oi)
	}

	return res, nil
}
