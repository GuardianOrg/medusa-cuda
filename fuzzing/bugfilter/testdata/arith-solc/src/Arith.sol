// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

contract Arith {
    uint256 public x;

    function checkedAdd(uint256 a) external {
        x = x + a;
    }

    function uncheckedOps(uint256 a) external {
        unchecked {
            x = x + a;
            x = x - a;
            x = x * a;
        }
    }

    function checkedSubMul(uint256 a) external {
        x = x - a;
        x = x * a;
    }

    function asmAdd(uint256 a) external {
        assembly {
            sstore(x.slot, add(sload(x.slot), a))
        }
    }
}
