# shellcheck shell=bash disable=SC2034 # bench/bench.sh reads about, disk and needs
# Repeats the blockcost suite's 16 GiB hit cell, which set the price, with its
# miss cell and a cell with a 2 s gap between blocks. A 20 GiB cell shows what
# more memory buys. The suite reuses the blockcost seed.
about="The blockcost suite's 16 GB cell again, and 20 GB"
disk=1
needs=(blockcost)

# The cells and labels match the recorded reference run, so
# `bench/bench.sh compare` pairs a new run with it cell by cell.
cells() {
    blockcost_cell p2-hit-16g-rep 16G -names 143198 -mode hit -label hit-16g-rep
    blockcost_cell p2-miss-16g 16G -names 143198 -mode miss -label miss-16g
    blockcost_cell p2-hit-16g-gap 16G -names 143198 -mode hit -gap 2s -label hit-16g-gap2s
    blockcost_cell p2-hit-20g 20G -names 143198 -mode hit -label hit-20g
}
