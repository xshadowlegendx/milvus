// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License

#pragma once

#include <memory>
#include <string>

#include "opentelemetry/trace/provider.h"

#define TRACE_SERVICE_SEGCORE "segcore"

namespace milvus::tracer {

struct TraceConfig {
    std::string exporter;
    float sampleFraction;
    std::string jaegerURL;
    std::string otlpEndpoint;
    bool oltpSecure;

    int nodeID;
};

struct TraceContext {
    const uint8_t* traceID = nullptr;
    const uint8_t* spanID = nullptr;
    uint8_t traceFlags = 0;
};
namespace trace = opentelemetry::trace;

void
initTelemetry(const TraceConfig& cfg);

std::shared_ptr<trace::Tracer>
GetTracer();

std::shared_ptr<trace::Span>
StartSpan(const std::string& name, TraceContext* ctx = nullptr);

void
SetRootSpan(std::shared_ptr<trace::Span> span);

void
CloseRootSpan();

void
AddEvent(const std::string& event_label);

bool
EmptyTraceID(const TraceContext* ctx);

bool
EmptySpanID(const TraceContext* ctx);

std::string
GetTraceIDAsStr(const TraceContext* ctx);

std::string
GetSpanIDAsStr(const TraceContext* ctx);

}  // namespace milvus::tracer
