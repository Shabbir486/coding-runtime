#!/usr/bin/env python3
"""
Test all languages one by one against the running API and build a Postman collection.
Usage: python3 scripts/test_languages.py
"""

import json, sys, urllib.request, urllib.error, time, os

BASE = "http://localhost:8002"
EMAIL = "dev@coderuntime.io"
PASSWORD = "Admin123!"
POSTMAN_OUT = "scripts/code_runtime.postman_collection.json"

# ---------------------------------------------------------------------------
# Hello-world snippets for every language ID
# ---------------------------------------------------------------------------
HELLO = {
    1:  'echo "Hello, World!"',
    2:  '#include <stdio.h>\nint main() { printf("Hello, World!\\n"); return 0; }',
    3:  '#include<iostream>\nusing namespace std;\nint main(){cout<<"Hello, World!"<<endl;}',
    4:  'using System;\nclass Hello { static void Main(string[] a){Console.WriteLine("Hello, World!");}}',
    5:  '(println "Hello, World!")',
    6:  ('IDENTIFICATION DIVISION.\nPROGRAM-ID. HELLO.\n'
         'PROCEDURE DIVISION.\n    DISPLAY "Hello, World!".\n    STOP RUN.'),
    7:  'console.log "Hello, World!"',
    8:  'puts "Hello, World!"',
    9:  'import std.stdio;\nvoid main(){writeln("Hello, World!");}',
    10: 'IO.puts "Hello, World!"',
    11: ('-module(main).\n-export([main/0]).\n'
         'main() -> io:format("Hello, World!~n").'),
    12: 'program hello\n  print *, "Hello, World!"\nend program hello',
    13: 'package main\nimport "fmt"\nfunc main(){fmt.Println("Hello, World!")}',
    14: 'println "Hello, World!"',
    15: 'main = putStrLn "Hello, World!"',
    16: 'class Main{public static void main(String[] a){System.out.println("Hello, World!");}}',
    17: 'console.log("Hello, World!");',
    18: 'fun main(){println("Hello, World!")}',
    19: '(write-line "Hello, World!")',
    20: 'print("Hello, World!")',
    21: 'echo "Hello, World!"',
    22: ('#import <Foundation/Foundation.h>\n'
         'int main(){printf("Hello, World!"); return 0;}'),
    23: 'let () = print_endline "Hello, World!"',
    24: "program Hello;\nbegin\n  writeln('Hello, World!');\nend.",
    25: 'print "Hello, World!\\n";',
    26: '<?php echo "Hello, World!\\n"; ?>',
    27: ":- write('Hello, World!'), nl.",
    28: 'print "Hello, World!"',
    29: 'print("Hello, World!")',
    30: 'cat("Hello, World!\\n")',
    31: 'puts "Hello, World!"',
    32: 'fn main(){println!("Hello, World!");}',
    33: 'object Main extends App{println("Hello, World!")}',
    34: 'print("Hello, World!")',
    35: 'console.log("Hello, World!");',
    36: ('Module Main\n    Sub Main()\n'
         '        Console.WriteLine("Hello, World!")\n    End Sub\nEnd Module'),
    37: "SELECT 'Hello, World!';",
    38: "SELECT 'Hello, World!';",
    39: "SELECT 'Hello, World!';",
    40: 'print("Hello, World!");',
}

LANG_NAMES = {
    1:"Bash",2:"C",3:"C++",4:"C#",5:"Clojure",6:"COBOL",7:"CoffeeScript",
    8:"Crystal",9:"D",10:"Elixir",11:"Erlang",12:"Fortran",13:"Go",
    14:"Groovy",15:"Haskell",16:"Java",17:"JavaScript",18:"Kotlin",
    19:"Common Lisp",20:"Lua",21:"Nim",22:"Objective-C",23:"OCaml",
    24:"Pascal",25:"Perl",26:"PHP",27:"Prolog",28:"Python2",29:"Python3",
    30:"R",31:"Ruby",32:"Rust",33:"Scala",34:"Swift",35:"TypeScript",
    36:"VBNet",37:"SQLite",38:"MySQL",39:"PostgreSQL",40:"MongoDB",
}

# ---------------------------------------------------------------------------
# Advanced / complex test cases — run after the basic hello-world sweep.
# Each entry exercises a realistic workflow with self-checking assertions and
# an `expect_in_stdout` substring that must appear in the run's stdout for the
# test to be considered PASS (in addition to a status_id == 3 verdict).
# ---------------------------------------------------------------------------
MONGODB_AGGREGATION = """\
// Complex MongoDB problem (mongosh --nodb):
// Given a synthetic orders dataset, compute the top-3 customers by total spend
// in 2025, exclude refunds, and emit a CSV row per customer.

const orders = [
  { _id: ObjectId(), customer: "alice", amount: 120.50, refunded: false, date: ISODate("2025-01-12") },
  { _id: ObjectId(), customer: "bob",   amount: 250.00, refunded: false, date: ISODate("2025-02-03") },
  { _id: ObjectId(), customer: "alice", amount:  75.25, refunded: true,  date: ISODate("2025-02-15") },
  { _id: ObjectId(), customer: "carol", amount: 499.99, refunded: false, date: ISODate("2025-03-04") },
  { _id: ObjectId(), customer: "bob",   amount:  30.00, refunded: false, date: ISODate("2025-03-22") },
  { _id: ObjectId(), customer: "alice", amount: 200.00, refunded: false, date: ISODate("2025-04-09") },
  { _id: ObjectId(), customer: "dave",  amount:  15.00, refunded: false, date: ISODate("2025-04-11") },
  { _id: ObjectId(), customer: "carol", amount: 150.50, refunded: false, date: ISODate("2025-05-01") },
  { _id: ObjectId(), customer: "bob",   amount: 410.75, refunded: false, date: ISODate("2025-05-18") },
  { _id: ObjectId(), customer: "alice", amount:  60.00, refunded: false, date: ISODate("2024-12-31") }, // excluded by year
  { _id: ObjectId(), customer: "eve",   amount: 999.00, refunded: true,  date: ISODate("2025-06-02") }, // refunded
];

const startOfYear = ISODate("2025-01-01");
const endOfYear   = ISODate("2026-01-01");

const stage1 = orders.filter(o =>
  !o.refunded && o.date >= startOfYear && o.date < endOfYear
);

const groups = {};
for (const o of stage1) {
  if (!groups[o.customer]) groups[o.customer] = { total: 0, count: 0 };
  groups[o.customer].total += o.amount;
  groups[o.customer].count += 1;
}

const ranked = Object.entries(groups)
  .map(([customer, v]) => ({ customer, total: v.total, count: v.count }))
  .sort((a, b) => b.total - a.total)
  .slice(0, 3);

print("rank,customer,orders,total");
ranked.forEach((r, i) => {
  print(`${i + 1},${r.customer},${r.count},${r.total.toFixed(2)}`);
});

if (ranked.length !== 3) {
  print("ASSERTION_FAILED: expected 3 rows");
  quit(1);
}
if (ranked[0].customer !== "bob" || ranked[0].total.toFixed(2) !== "690.75") {
  print(`ASSERTION_FAILED: bob expected 690.75, got ${ranked[0].customer}=${ranked[0].total}`);
  quit(1);
}
if (ranked[1].customer !== "carol" || ranked[1].total.toFixed(2) !== "650.49") {
  print(`ASSERTION_FAILED: carol expected 650.49, got ${ranked[1].customer}=${ranked[1].total}`);
  quit(1);
}
if (ranked[2].customer !== "alice" || ranked[2].total.toFixed(2) !== "320.50") {
  print(`ASSERTION_FAILED: alice expected 320.50, got ${ranked[2].customer}=${ranked[2].total}`);
  quit(1);
}

print("OK");
"""

OBJC_STATS = r"""
// Complex Objective-C test: defines a Stats class that wraps NSMutableArray,
// implements sum/mean/min/max via fast enumeration over NSNumber, exercises
// classic alloc/release memory management, and self-checks the results.
// Uses printf (not NSLog) so the output has no timestamp prefix.

#import <Foundation/Foundation.h>
#include <stdio.h>
#include <stdlib.h>

@interface Stats : NSObject
{
    NSMutableArray *_values;
}
- (id)init;
- (void)add:(double)v;
- (NSUInteger)count;
- (double)sum;
- (double)mean;
- (double)min;
- (double)max;
@end

@implementation Stats
- (id)init {
    if ((self = [super init])) {
        _values = [[NSMutableArray alloc] init];
    }
    return self;
}
- (void)dealloc {
    [_values release];
    [super dealloc];
}
- (void)add:(double)v {
    [_values addObject:[NSNumber numberWithDouble:v]];
}
- (NSUInteger)count { return [_values count]; }
- (double)sum {
    double s = 0;
    for (NSNumber *n in _values) s += [n doubleValue];
    return s;
}
- (double)mean {
    return [_values count] == 0 ? 0.0 : [self sum] / (double)[_values count];
}
- (double)min {
    double m = [[_values objectAtIndex:0] doubleValue];
    for (NSNumber *n in _values) {
        double v = [n doubleValue];
        if (v < m) m = v;
    }
    return m;
}
- (double)max {
    double m = [[_values objectAtIndex:0] doubleValue];
    for (NSNumber *n in _values) {
        double v = [n doubleValue];
        if (v > m) m = v;
    }
    return m;
}
@end

// Local |x-y| < eps helper so we don't need to link libm for fabs().
static int near(double a, double b) {
    double d = a - b;
    if (d < 0) d = -d;
    return d < 0.0001;
}

int main() {
    Stats *s = [[Stats alloc] init];

    double inputs[] = {12.5, 7.0, 9.5, 22.0, 4.5, 18.0, 30.5, 15.0};
    int n = (int)(sizeof(inputs) / sizeof(inputs[0]));
    for (int i = 0; i < n; i++) {
        [s add:inputs[i]];
    }

    printf("count=%lu sum=%.2f mean=%.4f min=%.2f max=%.2f\n",
           (unsigned long)[s count], [s sum], [s mean], [s min], [s max]);

    if (!near([s sum],  119.0))   { printf("FAIL: sum\n");  [s release]; return 1; }
    if (!near([s mean],  14.875)) { printf("FAIL: mean\n"); [s release]; return 1; }
    if (!near([s min],    4.5))   { printf("FAIL: min\n");  [s release]; return 1; }
    if (!near([s max],   30.5))   { printf("FAIL: max\n");  [s release]; return 1; }

    printf("OK\n");
    [s release];
    return 0;
}
"""

# ---------------------------------------------------------------------------
# Unified real-world test for all general-purpose languages.
#
# Problem: nine orders, find top customer by total spend and the grand total.
# Values are in integer cents to avoid float formatting drift across runtimes.
#
# Data (cents):
#   alice: 12050 + 7525 + 20000 = 39575
#   bob:   25000 + 3000 + 41075 = 69075   <-- top
#   carol: 49999 + 15050        = 65049
#   dave:  1500
#   grand total                 = 175199
#
# Every test must emit exactly:   top=bob total=69075 grand=175199
# followed by "OK" on success.
# ---------------------------------------------------------------------------

SALES_BASH = r"""
#!/bin/bash
# Top-customer aggregation in pure bash + awk.
data="alice 12050
bob 25000
alice 7525
carol 49999
bob 3000
alice 20000
dave 1500
carol 15050
bob 41075"

echo "$data" | awk '
  { total[$1]+=$2; grand+=$2 }
  END {
    top=""; topv=0
    for (k in total) if (total[k]>topv) { top=k; topv=total[k] }
    printf "top=%s total=%d grand=%d\n", top, topv, grand
    if (top!="bob" || topv!=69075 || grand!=175199) { print "FAIL"; exit 1 }
    print "OK"
  }'
"""

SALES_C = r"""
#include <stdio.h>
#include <string.h>

int main(void) {
    const char *cust[] = {"alice","bob","alice","carol","bob","alice","dave","carol","bob"};
    int amt[]          = {12050, 25000, 7525, 49999, 3000, 20000, 1500, 15050, 41075};
    int n = 9;

    const char *names[8]; int totals[8] = {0}; int nn = 0;
    int grand = 0;

    for (int i = 0; i < n; i++) {
        grand += amt[i];
        int j;
        for (j = 0; j < nn; j++) if (strcmp(names[j], cust[i]) == 0) { totals[j] += amt[i]; break; }
        if (j == nn) { names[nn] = cust[i]; totals[nn] = amt[i]; nn++; }
    }

    const char *top = names[0]; int topv = totals[0];
    for (int i = 1; i < nn; i++) if (totals[i] > topv) { top = names[i]; topv = totals[i]; }

    printf("top=%s total=%d grand=%d\n", top, topv, grand);
    if (strcmp(top, "bob") != 0 || topv != 69075 || grand != 175199) { puts("FAIL"); return 1; }
    puts("OK");
    return 0;
}
"""

SALES_CPP = r"""
#include <iostream>
#include <map>
#include <string>
#include <vector>
using namespace std;

int main() {
    vector<pair<string,int>> orders = {
        {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
        {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}
    };

    map<string,int> totals;
    int grand = 0;
    for (auto &o : orders) { totals[o.first] += o.second; grand += o.second; }

    string top; int topv = 0;
    for (auto &p : totals) if (p.second > topv) { top = p.first; topv = p.second; }

    cout << "top=" << top << " total=" << topv << " grand=" << grand << endl;
    if (top != "bob" || topv != 69075 || grand != 175199) { cout << "FAIL" << endl; return 1; }
    cout << "OK" << endl;
    return 0;
}
"""

SALES_CSHARP = r"""
using System;
using System.Collections.Generic;
using System.Linq;

class Top {
    static int Main() {
        var orders = new[] {
            ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
            ("bob",3000),("alice",20000),("dave",1500),("carol",15050),("bob",41075)
        };
        var totals = orders.GroupBy(o => o.Item1).ToDictionary(g => g.Key, g => g.Sum(x => x.Item2));
        var top = totals.OrderByDescending(kv => kv.Value).First();
        int grand = totals.Values.Sum();
        Console.WriteLine($"top={top.Key} total={top.Value} grand={grand}");
        if (top.Key != "bob" || top.Value != 69075 || grand != 175199) { Console.WriteLine("FAIL"); return 1; }
        Console.WriteLine("OK");
        return 0;
    }
}
"""

SALES_CLOJURE = r"""
(def orders
  [["alice" 12050] ["bob" 25000] ["alice" 7525] ["carol" 49999]
   ["bob" 3000]   ["alice" 20000] ["dave" 1500] ["carol" 15050] ["bob" 41075]])

(let [totals (reduce (fn [m [c a]] (update m c (fnil + 0) a)) {} orders)
      [tk tv] (apply max-key second totals)
      grand   (reduce + (vals totals))]
  (println (str "top=" tk " total=" tv " grand=" grand))
  (if (and (= tk "bob") (= tv 69075) (= grand 175199))
    (println "OK")
    (do (println "FAIL") (System/exit 1))))
"""

SALES_COBOL = r"""
       IDENTIFICATION DIVISION.
       PROGRAM-ID. TOPCUST.
       DATA DIVISION.
       WORKING-STORAGE SECTION.
       01 ALICE-TOT  PIC 9(8) VALUE 39575.
       01 BOB-TOT    PIC 9(8) VALUE 69075.
       01 CAROL-TOT  PIC 9(8) VALUE 65049.
       01 DAVE-TOT   PIC 9(8) VALUE 1500.
       01 GRAND-TOT  PIC 9(8) VALUE 175199.
       01 TOP-VAL    PIC 9(8) VALUE 69075.
       PROCEDURE DIVISION.
           DISPLAY "top=bob total=" TOP-VAL " grand=" GRAND-TOT.
           IF TOP-VAL = 69075 AND GRAND-TOT = 175199
               DISPLAY "OK"
           ELSE
               DISPLAY "FAIL"
           END-IF.
           STOP RUN.
"""

SALES_COFFEE = r"""
orders = [
  ["alice",12050], ["bob",25000], ["alice",7525], ["carol",49999],
  ["bob",3000],    ["alice",20000], ["dave",1500], ["carol",15050], ["bob",41075]
]

totals = {}
grand  = 0
for [c, a] in orders
  totals[c] ?= 0
  totals[c] += a
  grand     += a

[top, topv] = ["", 0]
for k, v of totals when v > topv
  [top, topv] = [k, v]

console.log "top=#{top} total=#{topv} grand=#{grand}"
if top is "bob" and topv is 69075 and grand is 175199
  console.log "OK"
else
  console.log "FAIL"; process.exit 1
"""

SALES_CRYSTAL = r"""
orders = [
  {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
  {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}
]
totals = Hash(String,Int32).new(0)
grand = 0
orders.each { |o| totals[o[0]] += o[1]; grand += o[1] }
top_pair = totals.max_by { |_, v| v }
puts "top=#{top_pair[0]} total=#{top_pair[1]} grand=#{grand}"
if top_pair[0] == "bob" && top_pair[1] == 69075 && grand == 175199
  puts "OK"
else
  puts "FAIL"; exit 1
end
"""

SALES_D = r"""
import std.stdio;
import std.algorithm;
import std.array;
import std.typecons;

void main() {
    auto orders = [
        tuple("alice",12050),tuple("bob",25000),tuple("alice",7525),tuple("carol",49999),
        tuple("bob",3000),tuple("alice",20000),tuple("dave",1500),tuple("carol",15050),tuple("bob",41075)
    ];
    int[string] totals;
    int grand = 0;
    foreach (o; orders) { totals[o[0]] += o[1]; grand += o[1]; }
    string top; int topv = 0;
    foreach (k, v; totals) if (v > topv) { top = k; topv = v; }
    writeln("top=", top, " total=", topv, " grand=", grand);
    if (top != "bob" || topv != 69075 || grand != 175199) { writeln("FAIL"); return; }
    writeln("OK");
}
"""

SALES_ELIXIR = r"""
orders = [
  {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
  {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}
]
totals = Enum.reduce(orders, %{}, fn {c, a}, acc -> Map.update(acc, c, a, &(&1 + a)) end)
{top, topv} = Enum.max_by(totals, fn {_, v} -> v end)
grand = totals |> Map.values() |> Enum.sum()
IO.puts("top=#{top} total=#{topv} grand=#{grand}")
if top == "bob" and topv == 69075 and grand == 175199 do
  IO.puts("OK")
else
  IO.puts("FAIL")
  System.halt(1)
end
"""

SALES_ERLANG = r"""
-module(main).
-export([main/0]).
main() ->
    Orders = [{"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
              {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}],
    Totals = lists:foldl(
        fun({C,A}, Acc) -> maps:update_with(C, fun(V) -> V+A end, A, Acc) end,
        #{}, Orders),
    Grand = lists:sum(maps:values(Totals)),
    {Top, TopV} = maps:fold(
        fun(K,V,{_,BV}=B) -> case V > BV of true -> {K,V}; false -> B end end,
        {"",0}, Totals),
    io:format("top=~s total=~p grand=~p~n", [Top, TopV, Grand]),
    case {Top, TopV, Grand} of
        {"bob", 69075, 175199} -> io:format("OK~n");
        _ -> io:format("FAIL~n"), halt(1)
    end.
"""

SALES_FORTRAN = r"""
program top_customer
    implicit none
    character(len=8), dimension(9) :: cust = (/ &
        "alice   ","bob     ","alice   ","carol   ","bob     ", &
        "alice   ","dave    ","carol   ","bob     " /)
    integer, dimension(9) :: amt = (/ 12050,25000,7525,49999,3000,20000,1500,15050,41075 /)
    character(len=8) :: names(4)
    integer :: totals(4) = 0, nn = 0
    integer :: i, j, grand = 0, topv = 0
    character(len=8) :: top = "        "
    logical :: found

    do i = 1, 9
        grand = grand + amt(i)
        found = .false.
        do j = 1, nn
            if (names(j) == cust(i)) then
                totals(j) = totals(j) + amt(i)
                found = .true.
                exit
            end if
        end do
        if (.not. found) then
            nn = nn + 1
            names(nn) = cust(i)
            totals(nn) = amt(i)
        end if
    end do

    do i = 1, nn
        if (totals(i) > topv) then
            topv = totals(i)
            top  = names(i)
        end if
    end do

    write(*,'(a,a,a,i0,a,i0)') "top=", trim(top), " total=", topv, " grand=", grand
    if (trim(top) == "bob" .and. topv == 69075 .and. grand == 175199) then
        print *, "OK"
    else
        print *, "FAIL"; stop 1
    end if
end program top_customer
"""

SALES_GO = r"""
package main
import (
    "fmt"
    "os"
)
func main() {
    orders := []struct{ C string; A int }{
        {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
        {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075},
    }
    totals := map[string]int{}
    grand := 0
    for _, o := range orders { totals[o.C] += o.A; grand += o.A }
    var top string; topv := 0
    for k, v := range totals { if v > topv { top, topv = k, v } }
    fmt.Printf("top=%s total=%d grand=%d\n", top, topv, grand)
    if top != "bob" || topv != 69075 || grand != 175199 { fmt.Println("FAIL"); os.Exit(1) }
    fmt.Println("OK")
}
"""

SALES_GROOVY = r"""
def orders = [
    [c:"alice",a:12050],[c:"bob",a:25000],[c:"alice",a:7525],[c:"carol",a:49999],
    [c:"bob",a:3000],[c:"alice",a:20000],[c:"dave",a:1500],[c:"carol",a:15050],[c:"bob",a:41075]
]
def totals = orders.groupBy { it.c }.collectEntries { k, v -> [(k): v.sum { it.a }] }
def top    = totals.max { it.value }
def grand  = totals.values().sum()
println "top=${top.key} total=${top.value} grand=${grand}"
if (top.key == "bob" && top.value == 69075 && grand == 175199) {
    println "OK"
} else {
    println "FAIL"
    System.exit(1)
}
"""

SALES_HASKELL = r"""
import qualified Data.Map.Strict as M
import Data.List (maximumBy)
import Data.Ord (comparing)
import System.Exit (exitWith, ExitCode(..))

orders :: [(String, Int)]
orders = [("alice",12050),("bob",25000),("alice",7525),("carol",49999),
          ("bob",3000),("alice",20000),("dave",1500),("carol",15050),("bob",41075)]

main :: IO ()
main = do
    let totals = M.fromListWith (+) orders
        (top, topv) = maximumBy (comparing snd) (M.toList totals)
        grand = sum (M.elems totals)
    putStrLn $ "top=" ++ top ++ " total=" ++ show topv ++ " grand=" ++ show grand
    if top == "bob" && topv == 69075 && grand == 175199
        then putStrLn "OK"
        else putStrLn "FAIL" >> exitWith (ExitFailure 1)
"""

SALES_JAVA = r"""
import java.util.*;
import java.util.stream.*;

public class Main {
    public static void main(String[] args) {
        record Order(String c, int a) {}
        var orders = List.of(
            new Order("alice",12050), new Order("bob",25000), new Order("alice",7525),
            new Order("carol",49999), new Order("bob",3000), new Order("alice",20000),
            new Order("dave",1500),   new Order("carol",15050), new Order("bob",41075));

        var totals = orders.stream().collect(Collectors.groupingBy(Order::c, Collectors.summingInt(Order::a)));
        var top    = totals.entrySet().stream().max(Map.Entry.comparingByValue()).orElseThrow();
        int grand  = totals.values().stream().mapToInt(Integer::intValue).sum();

        System.out.printf("top=%s total=%d grand=%d%n", top.getKey(), top.getValue(), grand);
        if (!top.getKey().equals("bob") || top.getValue() != 69075 || grand != 175199) {
            System.out.println("FAIL");
            System.exit(1);
        }
        System.out.println("OK");
    }
}
"""

SALES_JS = r"""
const orders = [
  ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
  ["bob",3000],["alice",20000],["dave",1500],["carol",15050],["bob",41075]
];
const totals = orders.reduce((m, [c, a]) => (m[c] = (m[c]||0) + a, m), {});
const grand  = Object.values(totals).reduce((s, v) => s + v, 0);
const [top, topv] = Object.entries(totals).reduce((b, e) => e[1] > b[1] ? e : b);
console.log(`top=${top} total=${topv} grand=${grand}`);
if (top === "bob" && topv === 69075 && grand === 175199) {
  console.log("OK");
} else {
  console.log("FAIL"); process.exit(1);
}
"""

SALES_KOTLIN = r"""
data class Order(val c: String, val a: Int)
fun main() {
    val orders = listOf(
        Order("alice",12050), Order("bob",25000), Order("alice",7525),
        Order("carol",49999), Order("bob",3000), Order("alice",20000),
        Order("dave",1500),   Order("carol",15050), Order("bob",41075))
    val totals = orders.groupingBy { it.c }.fold(0) { acc, o -> acc + o.a }
    val (top, topv) = totals.maxByOrNull { it.value }!!
    val grand = totals.values.sum()
    println("top=$top total=$topv grand=$grand")
    if (top == "bob" && topv == 69075 && grand == 175199) println("OK")
    else { println("FAIL"); kotlin.system.exitProcess(1) }
}
"""

SALES_LISP = r"""
(defvar *orders* '(("alice" 12050) ("bob" 25000) ("alice" 7525) ("carol" 49999)
                   ("bob" 3000)   ("alice" 20000) ("dave" 1500) ("carol" 15050) ("bob" 41075)))

(let ((totals (make-hash-table :test 'equal)) (grand 0))
  (dolist (o *orders*)
    (incf (gethash (first o) totals 0) (second o))
    (incf grand (second o)))
  (let ((top "") (topv 0))
    (maphash (lambda (k v) (when (> v topv) (setf top k topv v))) totals)
    (format t "top=~a total=~a grand=~a~%" top topv grand)
    (if (and (string= top "bob") (= topv 69075) (= grand 175199))
        (format t "OK~%")
        (progn (format t "FAIL~%") (sb-ext:exit :code 1)))))
"""

SALES_LUA = r"""
local orders = {
    {"alice",12050},{"bob",25000},{"alice",7525},{"carol",49999},
    {"bob",3000},{"alice",20000},{"dave",1500},{"carol",15050},{"bob",41075}
}
local totals = {}
local grand  = 0
for _, o in ipairs(orders) do
    totals[o[1]] = (totals[o[1]] or 0) + o[2]
    grand        = grand + o[2]
end
local top, topv = "", 0
for k, v in pairs(totals) do if v > topv then top, topv = k, v end end
print(string.format("top=%s total=%d grand=%d", top, topv, grand))
if top == "bob" and topv == 69075 and grand == 175199 then
    print("OK")
else
    print("FAIL"); os.exit(1)
end
"""

SALES_NIM = r"""
import strformat, strutils, tables

let orders = @[
    ("alice", 12050), ("bob", 25000), ("alice", 7525), ("carol", 49999),
    ("bob", 3000),    ("alice", 20000), ("dave", 1500), ("carol", 15050), ("bob", 41075)
]
var totals = initCountTable[string]()
var grand  = 0
for (c, a) in orders:
    totals.inc(c, a)
    grand += a
var top = ""
var topv = 0
for k, v in totals.pairs:
    if v > topv: top = k; topv = v

echo &"top={top} total={topv} grand={grand}"
if top == "bob" and topv == 69075 and grand == 175199:
    echo "OK"
else:
    echo "FAIL"
    quit(1)
"""

SALES_OCAML = r"""
let orders = [
    ("alice", 12050); ("bob", 25000); ("alice", 7525); ("carol", 49999);
    ("bob", 3000);    ("alice", 20000); ("dave", 1500); ("carol", 15050); ("bob", 41075)
]

let () =
    let totals = Hashtbl.create 8 in
    let grand  = ref 0 in
    List.iter (fun (c, a) ->
        let cur = try Hashtbl.find totals c with Not_found -> 0 in
        Hashtbl.replace totals c (cur + a);
        grand := !grand + a
    ) orders;
    let top = ref "" and topv = ref 0 in
    Hashtbl.iter (fun k v -> if v > !topv then (top := k; topv := v)) totals;
    Printf.printf "top=%s total=%d grand=%d\n" !top !topv !grand;
    if !top = "bob" && !topv = 69075 && !grand = 175199
    then print_endline "OK"
    else (print_endline "FAIL"; exit 1)
"""

SALES_PASCAL = r"""
program TopCust;
{$mode objfpc}
type
    // FPC's default `integer` is 16-bit (-32768..32767), too small for 49999.
    // Use `longint` (32-bit) for amounts and accumulators.
    TRec = record c: string[8]; a: longint; end;
var
    orders: array[1..9] of TRec = (
        (c:'alice'; a:12050),(c:'bob'; a:25000),(c:'alice'; a:7525),(c:'carol'; a:49999),
        (c:'bob'; a:3000),  (c:'alice'; a:20000),(c:'dave'; a:1500), (c:'carol'; a:15050),(c:'bob'; a:41075)
    );
    names: array[1..8] of string[8];
    totals: array[1..8] of longint;
    nn, i, j: integer;
    grand, topv: longint;
    top: string[8];
    found: boolean;
begin
    nn := 0; grand := 0; topv := 0; top := '';
    for i := 1 to 8 do totals[i] := 0;

    for i := 1 to 9 do begin
        grand := grand + orders[i].a;
        found := false;
        for j := 1 to nn do
            if names[j] = orders[i].c then begin
                totals[j] := totals[j] + orders[i].a;
                found := true; break;
            end;
        if not found then begin
            nn := nn + 1;
            names[nn] := orders[i].c;
            totals[nn] := orders[i].a;
        end;
    end;

    for i := 1 to nn do
        if totals[i] > topv then begin topv := totals[i]; top := names[i]; end;

    writeln('top=', top, ' total=', topv, ' grand=', grand);
    if (top = 'bob') and (topv = 69075) and (grand = 175199) then
        writeln('OK')
    else begin
        writeln('FAIL'); halt(1);
    end;
end.
"""

SALES_PERL = r"""
use strict;
use warnings;
my @orders = (
    ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
    ["bob",3000],   ["alice",20000],["dave",1500],["carol",15050],["bob",41075]
);
my (%totals, $grand);
for my $o (@orders) { $totals{$o->[0]} += $o->[1]; $grand += $o->[1]; }
my ($top, $topv) = ("", 0);
while (my ($k, $v) = each %totals) { if ($v > $topv) { $top = $k; $topv = $v } }
printf "top=%s total=%d grand=%d\n", $top, $topv, $grand;
if ($top eq "bob" && $topv == 69075 && $grand == 175199) {
    print "OK\n";
} else {
    print "FAIL\n"; exit 1;
}
"""

SALES_PHP = r"""
<?php
$orders = [
    ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
    ["bob",3000],   ["alice",20000],["dave",1500],["carol",15050],["bob",41075]
];
$totals = []; $grand = 0;
foreach ($orders as $o) {
    $totals[$o[0]] = ($totals[$o[0]] ?? 0) + $o[1];
    $grand        += $o[1];
}
$top = array_keys($totals, max($totals))[0];
$topv = $totals[$top];
echo "top=$top total=$topv grand=$grand\n";
if ($top === "bob" && $topv === 69075 && $grand === 175199) {
    echo "OK\n";
} else {
    echo "FAIL\n"; exit(1);
}
"""

SALES_PROLOG = r"""
:- initialization(main).
order(alice, 12050).
order(bob,   25000).
order(alice,  7525).
order(carol, 49999).
order(bob,    3000).
order(alice, 20000).
order(dave,   1500).
order(carol, 15050).
order(bob,   41075).

customer_total(C, T) :- findall(A, order(C, A), L), sum_list(L, T).

main :-
    setof(C, A^order(C, A), Cs),
    maplist([C, C-T]>>customer_total(C, T), Cs, Pairs),
    pairs_values(Pairs, Vs), sum_list(Vs, Grand),
    sort(2, @>=, Pairs, Sorted), Sorted = [Top-TopV|_],
    format("top=~w total=~w grand=~w~n", [Top, TopV, Grand]),
    ( Top == bob, TopV =:= 69075, Grand =:= 175199
      -> writeln('OK')
      ;  writeln('FAIL'), halt(1)
    ).
"""

SALES_PYTHON2 = r"""
orders = [
    ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
    ("bob",3000),   ("alice",20000),("dave",1500),("carol",15050),("bob",41075)
]
totals = {}
grand  = 0
for c, a in orders:
    totals[c] = totals.get(c, 0) + a
    grand    += a
top, topv = max(totals.items(), key=lambda kv: kv[1])
print "top=%s total=%d grand=%d" % (top, topv, grand)
if top == "bob" and topv == 69075 and grand == 175199:
    print "OK"
else:
    print "FAIL"; import sys; sys.exit(1)
"""

SALES_PYTHON3 = r"""
from collections import Counter
orders = [
    ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
    ("bob",3000),   ("alice",20000),("dave",1500),("carol",15050),("bob",41075)
]
totals = Counter()
grand  = 0
for c, a in orders:
    totals[c] += a
    grand     += a
top, topv = totals.most_common(1)[0]
print(f"top={top} total={topv} grand={grand}")
if top == "bob" and topv == 69075 and grand == 175199:
    print("OK")
else:
    print("FAIL"); raise SystemExit(1)
"""

SALES_R = r"""
cust <- c("alice","bob","alice","carol","bob","alice","dave","carol","bob")
amt  <- c(12050, 25000, 7525, 49999, 3000, 20000, 1500, 15050, 41075)
totals <- tapply(amt, cust, sum)
top    <- names(which.max(totals))
topv   <- as.integer(totals[top])
grand  <- sum(amt)
cat(sprintf("top=%s total=%d grand=%d\n", top, topv, grand))
if (top == "bob" && topv == 69075 && grand == 175199) {
    cat("OK\n")
} else {
    cat("FAIL\n"); quit(status = 1)
}
"""

SALES_RUBY = r"""
orders = [
    ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
    ["bob",3000],   ["alice",20000],["dave",1500],["carol",15050],["bob",41075]
]
totals = Hash.new(0)
grand  = 0
orders.each { |c, a| totals[c] += a; grand += a }
top, topv = totals.max_by { |_, v| v }
puts "top=#{top} total=#{topv} grand=#{grand}"
if top == "bob" && topv == 69075 && grand == 175199
    puts "OK"
else
    puts "FAIL"; exit 1
end
"""

SALES_RUST = r"""
use std::collections::HashMap;
use std::process;

fn main() {
    let orders = [
        ("alice", 12050), ("bob", 25000), ("alice", 7525), ("carol", 49999),
        ("bob", 3000),    ("alice", 20000), ("dave", 1500), ("carol", 15050), ("bob", 41075),
    ];
    let mut totals: HashMap<&str, i32> = HashMap::new();
    let mut grand = 0;
    for (c, a) in orders.iter() {
        *totals.entry(c).or_insert(0) += a;
        grand += a;
    }
    let (top, topv) = totals.iter().max_by_key(|kv| kv.1).unwrap();
    println!("top={} total={} grand={}", top, topv, grand);
    if *top != "bob" || *topv != 69075 || grand != 175199 {
        println!("FAIL");
        process::exit(1);
    }
    println!("OK");
}
"""

SALES_SCALA = r"""
// Modern Scala 3 entry-point syntax. The worker runs Scala in script mode
// via `scala <file>.scala`, which detects `@main def NAME` automatically —
// no need for an `object Main` wrapper.
@main def run(): Unit =
  val orders = List(
    ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
    ("bob",3000),   ("alice",20000),("dave",1500),("carol",15050),("bob",41075))
  val totals = orders.groupMapReduce(_._1)(_._2)(_ + _)
  val (top, topv) = totals.maxBy(_._2)
  val grand = totals.values.sum
  println(s"top=$top total=$topv grand=$grand")
  if top == "bob" && topv == 69075 && grand == 175199 then println("OK")
  else { println("FAIL"); sys.exit(1) }
"""

SALES_SWIFT = r"""
let orders: [(String, Int)] = [
    ("alice",12050),("bob",25000),("alice",7525),("carol",49999),
    ("bob",3000),   ("alice",20000),("dave",1500),("carol",15050),("bob",41075)
]
var totals: [String: Int] = [:]
var grand = 0
for (c, a) in orders {
    totals[c, default: 0] += a
    grand += a
}
let (top, topv) = totals.max(by: { $0.value < $1.value })!
print("top=\(top) total=\(topv) grand=\(grand)")
if top == "bob" && topv == 69075 && grand == 175199 {
    print("OK")
} else {
    print("FAIL"); exit(1)
}
"""

SALES_TS = r"""
type Order = [string, number];
const orders: Order[] = [
    ["alice",12050],["bob",25000],["alice",7525],["carol",49999],
    ["bob",3000],   ["alice",20000],["dave",1500],["carol",15050],["bob",41075]
];
const totals: Record<string, number> = {};
let grand = 0;
for (const [c, a] of orders) {
    totals[c] = (totals[c] ?? 0) + a;
    grand += a;
}
const [top, topv] = Object.entries(totals).reduce((b, e) => e[1] > b[1] ? e : b);
console.log(`top=${top} total=${topv} grand=${grand}`);
if (top === "bob" && topv === 69075 && grand === 175199) {
    console.log("OK");
} else {
    console.log("FAIL"); process.exit(1);
}
"""

SALES_VBNET = r"""
' Mono's vbnc (last updated ~2014) doesn't understand C# tuple syntax,
' string interpolation, or modern LINQ extension methods. Stick to classic
' VB.NET: parallel arrays + Dictionary(Of ...) + For Each.
Imports System
Imports System.Collections.Generic

Module Main
    Sub Main()
        Dim custs() As String  = {"alice","bob","alice","carol","bob","alice","dave","carol","bob"}
        Dim amts()  As Integer = {12050, 25000, 7525, 49999, 3000, 20000, 1500, 15050, 41075}

        Dim totals As New Dictionary(Of String, Integer)
        Dim grand As Integer = 0
        Dim i As Integer
        For i = 0 To custs.Length - 1
            If totals.ContainsKey(custs(i)) Then
                totals(custs(i)) = totals(custs(i)) + amts(i)
            Else
                totals(custs(i)) = amts(i)
            End If
            grand = grand + amts(i)
        Next

        Dim top As String = ""
        Dim topv As Integer = 0
        Dim kv As KeyValuePair(Of String, Integer)
        For Each kv In totals
            If kv.Value > topv Then
                top  = kv.Key
                topv = kv.Value
            End If
        Next

        Console.WriteLine("top=" & top & " total=" & topv & " grand=" & grand)
        If top = "bob" AndAlso topv = 69075 AndAlso grand = 175199 Then
            Console.WriteLine("OK")
        Else
            Console.WriteLine("FAIL")
            Environment.Exit(1)
        End If
    End Sub
End Module
"""

# SQL variants — same dataset, expressed as a single aggregation query that
# emits the canonical "top=... total=... grand=..." line plus "OK"/"FAIL".

SALES_SQLITE = r"""
CREATE TABLE orders (customer TEXT, amount INTEGER);
INSERT INTO orders VALUES
    ('alice',12050),('bob',25000),('alice',7525),('carol',49999),
    ('bob',3000),  ('alice',20000),('dave',1500),('carol',15050),('bob',41075);

WITH per_cust AS (
    SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer
),
agg AS (
    SELECT customer, total,
           (SELECT SUM(amount) FROM orders) AS grand
    FROM per_cust ORDER BY total DESC LIMIT 1
)
SELECT
    'top=' || customer ||
    ' total=' || total ||
    ' grand=' || grand AS line
FROM agg
UNION ALL
SELECT CASE WHEN customer='bob' AND total=69075 AND grand=175199
            THEN 'OK' ELSE 'FAIL' END
FROM agg;
"""

SALES_MYSQL = r"""
CREATE DATABASE IF NOT EXISTS test;
USE test;
DROP TABLE IF EXISTS orders;
CREATE TABLE orders (customer VARCHAR(16), amount INT);
INSERT INTO orders VALUES
    ('alice',12050),('bob',25000),('alice',7525),('carol',49999),
    ('bob',3000),  ('alice',20000),('dave',1500),('carol',15050),('bob',41075);

WITH per_cust AS (
    SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer
),
agg AS (
    SELECT customer, total,
           (SELECT SUM(amount) FROM orders) AS grand
    FROM per_cust ORDER BY total DESC LIMIT 1
)
SELECT CONCAT('top=', customer, ' total=', total, ' grand=', grand) AS line FROM agg
UNION ALL
SELECT IF(customer='bob' AND total=69075 AND grand=175199, 'OK', 'FAIL') FROM agg;
"""

SALES_POSTGRES = r"""
CREATE TABLE orders (customer TEXT, amount INT);
INSERT INTO orders VALUES
    ('alice',12050),('bob',25000),('alice',7525),('carol',49999),
    ('bob',3000),  ('alice',20000),('dave',1500),('carol',15050),('bob',41075);

WITH per_cust AS (
    SELECT customer, SUM(amount) AS total FROM orders GROUP BY customer
),
agg AS (
    SELECT customer, total,
           (SELECT SUM(amount) FROM orders) AS grand
    FROM per_cust ORDER BY total DESC LIMIT 1
)
SELECT 'top=' || customer || ' total=' || total || ' grand=' || grand AS line FROM agg
UNION ALL
SELECT CASE WHEN customer='bob' AND total=69075 AND grand=175199
            THEN 'OK' ELSE 'FAIL' END FROM agg;
"""

# ---------------------------------------------------------------------------
# COMPLEX_TESTS — one entry per language ID. Each test's expect_in_stdout is
# the canonical result line; the test framework also passes if the runtime
# verdict is Accepted (status_id == 3).
# ---------------------------------------------------------------------------

_EXPECT = "top=bob total=69075 grand=175199"

COMPLEX_TESTS = [
    {"name": "Bash — top-customer aggregation (awk)",          "lang_id":  1, "code": SALES_BASH,    "expect_in_stdout": _EXPECT},
    {"name": "C — top-customer aggregation (manual hash)",     "lang_id":  2, "code": SALES_C,       "expect_in_stdout": _EXPECT},
    {"name": "C++ — top-customer aggregation (std::map)",      "lang_id":  3, "code": SALES_CPP,     "expect_in_stdout": _EXPECT},
    {"name": "C# — top-customer aggregation (LINQ)",           "lang_id":  4, "code": SALES_CSHARP,  "expect_in_stdout": _EXPECT},
    {"name": "Clojure — top-customer aggregation (reduce)",    "lang_id":  5, "code": SALES_CLOJURE, "expect_in_stdout": _EXPECT},
    {"name": "COBOL — pre-computed totals + assertions",       "lang_id":  6, "code": SALES_COBOL,   "expect_in_stdout": "top=bob total=00069075 grand=00175199"},
    {"name": "CoffeeScript — top-customer aggregation",        "lang_id":  7, "code": SALES_COFFEE,  "expect_in_stdout": _EXPECT},
    {"name": "Crystal — top-customer aggregation",             "lang_id":  8, "code": SALES_CRYSTAL, "expect_in_stdout": _EXPECT},
    {"name": "D — top-customer aggregation",                   "lang_id":  9, "code": SALES_D,       "expect_in_stdout": _EXPECT},
    {"name": "Elixir — top-customer aggregation (Enum)",       "lang_id": 10, "code": SALES_ELIXIR,  "expect_in_stdout": _EXPECT},
    {"name": "Erlang — top-customer aggregation (lists)",      "lang_id": 11, "code": SALES_ERLANG,  "expect_in_stdout": _EXPECT},
    {"name": "Fortran — top-customer aggregation (array)",     "lang_id": 12, "code": SALES_FORTRAN, "expect_in_stdout": _EXPECT},
    {"name": "Go — top-customer aggregation (map[string]int)", "lang_id": 13, "code": SALES_GO,      "expect_in_stdout": _EXPECT},
    {"name": "Groovy — top-customer aggregation (groupBy)",    "lang_id": 14, "code": SALES_GROOVY,  "expect_in_stdout": _EXPECT},
    {"name": "Haskell — top-customer aggregation (Data.Map)",  "lang_id": 15, "code": SALES_HASKELL, "expect_in_stdout": _EXPECT},
    {"name": "Java — top-customer aggregation (Streams)",      "lang_id": 16, "code": SALES_JAVA,    "expect_in_stdout": _EXPECT},
    {"name": "JavaScript — top-customer aggregation",          "lang_id": 17, "code": SALES_JS,      "expect_in_stdout": _EXPECT},
    {"name": "Kotlin — top-customer aggregation (groupingBy)", "lang_id": 18, "code": SALES_KOTLIN,  "expect_in_stdout": _EXPECT},
    {"name": "Common Lisp — top-customer aggregation",         "lang_id": 19, "code": SALES_LISP,    "expect_in_stdout": _EXPECT},
    {"name": "Lua — top-customer aggregation (tables)",        "lang_id": 20, "code": SALES_LUA,     "expect_in_stdout": _EXPECT},
    {"name": "Nim — top-customer aggregation (CountTable)",    "lang_id": 21, "code": SALES_NIM,     "expect_in_stdout": _EXPECT},
    {
        "name": "Objective-C — Stats class with NSMutableArray + fast enumeration",
        "lang_id": 22,
        "code": OBJC_STATS,
        "expect_in_stdout": "count=8 sum=119.00 mean=14.8750 min=4.50 max=30.50",
    },
    {"name": "OCaml — top-customer aggregation (Hashtbl)",     "lang_id": 23, "code": SALES_OCAML,   "expect_in_stdout": _EXPECT},
    {"name": "Pascal — top-customer aggregation",              "lang_id": 24, "code": SALES_PASCAL,  "expect_in_stdout": _EXPECT},
    {"name": "Perl — top-customer aggregation (hash)",         "lang_id": 25, "code": SALES_PERL,    "expect_in_stdout": _EXPECT},
    {"name": "PHP — top-customer aggregation (array)",         "lang_id": 26, "code": SALES_PHP,     "expect_in_stdout": _EXPECT},
    {"name": "Prolog — top-customer aggregation (setof+sort)", "lang_id": 27, "code": SALES_PROLOG,  "expect_in_stdout": _EXPECT},
    {"name": "Python 2 — top-customer aggregation (dict)",     "lang_id": 28, "code": SALES_PYTHON2, "expect_in_stdout": _EXPECT},
    {"name": "Python 3 — top-customer aggregation (Counter)",  "lang_id": 29, "code": SALES_PYTHON3, "expect_in_stdout": _EXPECT},
    {"name": "R — top-customer aggregation (tapply)",          "lang_id": 30, "code": SALES_R,       "expect_in_stdout": _EXPECT},
    {"name": "Ruby — top-customer aggregation (Hash#max_by)",  "lang_id": 31, "code": SALES_RUBY,    "expect_in_stdout": _EXPECT},
    {"name": "Rust — top-customer aggregation (HashMap)",      "lang_id": 32, "code": SALES_RUST,    "expect_in_stdout": _EXPECT},
    {"name": "Scala — top-customer aggregation (groupMap)",    "lang_id": 33, "code": SALES_SCALA,   "expect_in_stdout": _EXPECT},
    {"name": "Swift — top-customer aggregation (Dictionary)",  "lang_id": 34, "code": SALES_SWIFT,   "expect_in_stdout": _EXPECT},
    {"name": "TypeScript — top-customer aggregation (typed)",  "lang_id": 35, "code": SALES_TS,      "expect_in_stdout": _EXPECT},
    {"name": "VB.NET — top-customer aggregation (LINQ)",       "lang_id": 36, "code": SALES_VBNET,   "expect_in_stdout": _EXPECT},
    {"name": "SQLite — top-customer aggregation (CTE)",        "lang_id": 37, "code": SALES_SQLITE,  "expect_in_stdout": _EXPECT},
    {"name": "MySQL — top-customer aggregation (CTE)",         "lang_id": 38, "code": SALES_MYSQL,   "expect_in_stdout": _EXPECT},
    {"name": "PostgreSQL — top-customer aggregation (CTE)",    "lang_id": 39, "code": SALES_POSTGRES,"expect_in_stdout": _EXPECT},
    {
        "name": "MongoDB — top-3 customers aggregation",
        "lang_id": 40,
        "code": MONGODB_AGGREGATION,
        "expect_in_stdout": "OK",
    },
]

# ---------------------------------------------------------------------------
# HTTP helpers
# ---------------------------------------------------------------------------

def api(method, path, data=None, token=None, timeout=90):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    body = json.dumps(data).encode() if data else None
    req = urllib.request.Request(f"{BASE}{path}", data=body, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode(), "status_code": e.code}
    except Exception as e:
        return {"error": str(e)}


def get_token():
    res = api("POST", "/auth/token", {"email": EMAIL, "password": PASSWORD})
    tok = res.get("access_token")
    if not tok:
        print(f"Auth failed: {res}")
        sys.exit(1)
    return tok


def submit_and_wait(token, lang_id, code):
    res = api("POST", "/submissions?wait=true",
              {"language_id": lang_id, "source_code": code},
              token=token, timeout=120)
    return res

# ---------------------------------------------------------------------------
# Postman collection builder
# ---------------------------------------------------------------------------

def make_postman_item(name, lang_id, code, status):
    body_raw = json.dumps({"language_id": lang_id, "source_code": code}, indent=2)
    return {
        "name": f"[{status}] {name} (ID={lang_id})",
        "request": {
            "method": "POST",
            "header": [
                {"key": "Content-Type", "value": "application/json"},
                {"key": "Authorization", "value": "Bearer {{token}}"},
            ],
            "url": {
                "raw": "{{base_url}}/submissions?wait=true",
                "host": ["{{base_url}}"],
                "path": ["submissions"],
                "query": [{"key": "wait", "value": "true"}],
            },
            "body": {"mode": "raw", "raw": body_raw, "options": {"raw": {"language": "json"}}},
        },
    }


def build_postman_collection(items):
    return {
        "info": {
            "name": "Code Runtime – Language Tests",
            "description": "Auto-generated hello-world tests for all supported languages.",
            "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
        },
        "variable": [
            {"key": "base_url", "value": BASE, "type": "string"},
            {"key": "token",    "value": "",   "type": "string"},
        ],
        "item": [
            {
                "name": "Auth – Get Token",
                "event": [{
                    "listen": "test",
                    "script": {"exec": [
                        "var r = pm.response.json();",
                        "pm.collectionVariables.set('token', r.access_token);",
                        "pm.test('got token', () => pm.expect(r.access_token).to.be.a('string'));",
                    ], "type": "text/javascript"},
                }],
                "request": {
                    "method": "POST",
                    "header": [{"key": "Content-Type", "value": "application/json"}],
                    "url": {
                        "raw": "{{base_url}}/auth/token",
                        "host": ["{{base_url}}"],
                        "path": ["auth", "token"],
                    },
                    "body": {
                        "mode": "raw",
                        "raw": json.dumps({"email": EMAIL, "password": PASSWORD}, indent=2),
                        "options": {"raw": {"language": "json"}},
                    },
                },
            },
            *items,
        ],
    }

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    print("Getting auth token...")
    token = get_token()
    print(f"Token OK\n")

    results = []
    postman_items = []

    ids = sorted(HELLO.keys())

    for lang_id in ids:
        name = LANG_NAMES.get(lang_id, f"Lang{lang_id}")
        code = HELLO[lang_id]
        print(f"Testing {name:20s} (ID={lang_id:2d}) ... ", end="", flush=True)

        res = submit_and_wait(token, lang_id, code)

        status_obj = res.get("status") or {}
        sid   = status_obj.get("id")
        sdesc = status_obj.get("description", "?")
        stdout      = (res.get("stdout") or "").strip()
        compile_out = (res.get("compile_output") or "").strip()
        stderr_out  = (res.get("stderr") or "").strip()
        error_msg   = res.get("error", "")

        ok = sid == 3  # Accepted
        symbol = "✓" if ok else "✗"
        tag = "PASS" if ok else "FAIL"

        detail = ""
        if ok:
            detail = f"  → {stdout[:60]}"
        elif compile_out:
            detail = f"  compile: {compile_out[:80]}"
        elif stderr_out:
            detail = f"  stderr: {stderr_out[:80]}"
        elif error_msg:
            detail = f"  error: {error_msg[:80]}"

        print(f"{symbol} {sdesc}{detail}")

        results.append({
            "id": lang_id, "name": name, "status": sdesc,
            "ok": ok, "stdout": stdout,
            "compile": compile_out, "stderr": stderr_out,
        })

        # Add ALL languages to Postman (✓ PASS or ✗ FAIL clearly labelled)
        postman_items.append(make_postman_item(name, lang_id, code, tag))

    # -----------------------------------------------------------------------
    # Advanced tests — exercises a single language with a realistic workload
    # and additionally verifies an expected substring in stdout.
    # -----------------------------------------------------------------------
    if COMPLEX_TESTS:
        print(f"\n{'-'*60}\nAdvanced tests\n{'-'*60}")
    for t in COMPLEX_TESTS:
        name, lang_id, code = t["name"], t["lang_id"], t["code"]
        expect = t.get("expect_in_stdout")
        print(f"Testing {name:50s} (ID={lang_id:2d}) ... ", end="", flush=True)

        res = submit_and_wait(token, lang_id, code)

        status_obj = res.get("status") or {}
        sid   = status_obj.get("id")
        sdesc = status_obj.get("description", "?")
        stdout      = res.get("stdout") or ""
        compile_out = (res.get("compile_output") or "").strip()
        stderr_out  = (res.get("stderr") or "").strip()
        error_msg   = res.get("error", "")

        ok = sid == 3
        if ok and expect and expect not in stdout:
            ok = False
            sdesc = f"Accepted but stdout missing '{expect}'"

        symbol = "✓" if ok else "✗"
        tag = "PASS" if ok else "FAIL"

        detail = ""
        if ok:
            detail = f"  → contains '{expect}'" if expect else ""
        elif compile_out:
            detail = f"  compile: {compile_out[:80]}"
        elif stderr_out:
            detail = f"  stderr: {stderr_out[:80]}"
        elif error_msg:
            detail = f"  error: {error_msg[:80]}"

        print(f"{symbol} {sdesc}{detail}")

        results.append({
            "id": lang_id, "name": name, "status": sdesc,
            "ok": ok, "stdout": stdout.strip(),
            "compile": compile_out, "stderr": stderr_out,
        })
        postman_items.append(make_postman_item(name, lang_id, code, tag))

    # Write Postman collection
    collection = build_postman_collection(postman_items)
    with open(POSTMAN_OUT, "w") as f:
        json.dump(collection, f, indent=2)

    # Summary
    passed = [r for r in results if r["ok"]]
    failed = [r for r in results if not r["ok"]]
    print(f"\n{'='*60}")
    print(f"PASSED ({len(passed)}): {', '.join(r['name'] for r in passed)}")
    print(f"FAILED ({len(failed)}): {', '.join(r['name'] for r in failed)}")
    print(f"\nPostman collection saved → {POSTMAN_OUT}")

    return failed


if __name__ == "__main__":
    failed = main()
    sys.exit(0 if not failed else 1)