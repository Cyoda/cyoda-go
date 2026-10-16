package probe

import com.fasterxml.jackson.annotation.JsonInclude
import com.fasterxml.jackson.annotation.JsonSubTypes
import com.fasterxml.jackson.core.JsonPointer
import com.fasterxml.jackson.databind.DeserializationFeature
import com.fasterxml.jackson.databind.JsonNode
import com.fasterxml.jackson.databind.ObjectMapper
import com.fasterxml.jackson.databind.SerializationFeature
import com.fasterxml.jackson.databind.exc.InvalidTypeIdException
import com.fasterxml.jackson.databind.exc.UnrecognizedPropertyException
import com.fasterxml.jackson.databind.node.ArrayNode
import com.fasterxml.jackson.databind.node.ObjectNode
import com.fasterxml.jackson.module.kotlin.registerKotlinModule
import org.junit.jupiter.api.Assumptions.assumeTrue
import org.junit.jupiter.api.DynamicTest
import org.junit.jupiter.api.TestFactory
import org.opentest4j.TestAbortedException
import org.openapitools.model.AuditEventDto
import org.openapitools.model.EntityAuditEventsResponseDto
import org.openapitools.model.StateMachineAuditEventDto
import org.openapitools.model.StateMachineEventDto
import java.io.File
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue
import kotlin.test.fail

// Reads and writes every sample with the classes kotlin-spring generates with
// Cloud's options (package org.openapitools.model).
class KotlinSamplesTest {
    private val mapper: ObjectMapper = ObjectMapper().registerKotlinModule().findAndRegisterModules()
        .setSerializationInclusion(JsonInclude.Include.NON_NULL)
        .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS)
        .disable(DeserializationFeature.ADJUST_DATES_TO_CONTEXT_TIME_ZONE)

    // The two settings Rule 6 requires of a client: unknown fields and unknown
    // discriminator values are read as nothing.
    private val tolerant: ObjectMapper = mapper.copy()
        .disable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
        .disable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE)

    private val strict: ObjectMapper = mapper.copy()
        .enable(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES)
        .enable(DeserializationFeature.FAIL_ON_INVALID_SUBTYPE)

    @TestFactory
    fun samples(): List<DynamicTest> {
        val dir = File(System.getProperty("samples.dir") ?: "samples")
        val files = dir.listFiles { f -> f.name.endsWith(".json") }?.sortedBy { it.name }
        assertTrue(!files.isNullOrEmpty(), "no samples in $dir")
        return files.map { f ->
            DynamicTest.dynamicTest(f.name) {
                try {
                    check(mapper.readTree(f))
                } catch (e: TestAbortedException) {
                    throw e
                } catch (e: Throwable) {
                    throw AssertionError("${f.name}: ${e.message}", e)
                }
            }
        }
    }

    private fun check(sample: JsonNode) {
        // A create or import body is not a component schema: the generated
        // clients take it as an untyped value, so there is no class to read it as.
        assumeTrue(!sample.has("body"), "a request body sample")
        assumeTrue(sample.path("valid").asBoolean(true), "an invalid sample")
        val value = sample.path("value")
        val cls = Class.forName("org.openapitools.model." + sample.path("schema").asText())
        if (sample.path("future").asBoolean(false)) {
            future(cls, sample)
            return
        }
        val expected = assertNotNull(sample.path("class").textValue(), "a valid sample names its class")
        val read: Any = mapper.readValue(value.toString(), cls)
        assertEquals(expected, read.javaClass.simpleName, "the class read")
        assertEquals(withoutNullMembers(value), withoutNullMembers(mapper.readTree(mapper.writeValueAsString(read))), "the value written back")
    }

    // A future sample is an audit page that a later spec could send. Rule 6:
    // read with the two settings, an unknown audit kind is a null entry in
    // items, an unknown event type keeps eventType and has null data, and a
    // known event is read with exactly the sample's added members dropped.
    // Without the two settings, the read fails.
    private fun future(cls: Class<*>, sample: JsonNode) {
        assertEquals(EntityAuditEventsResponseDto::class.java, cls, "a future sample is an audit page")
        val value = sample.path("value")
        val declared = withoutAdded(value, sample.path("added"))
        val page = tolerant.readValue(value.toString(), EntityAuditEventsResponseDto::class.java)
        val items = assertNotNull(page.items)
        assertEquals(value.path("items").size(), items.size, "the number of items")
        var unknownMember = false
        value.path("items").forEachIndexed { i, sent ->
            val item: AuditEventDto? = items[i]
            when {
                !known(AuditEventDto::class.java, sent.path("auditEventType").asText()) -> {
                    unknownMember = true
                    assertNull(item, "an audit event of an unknown kind")
                }
                sent.path("auditEventType").asText() == "StateMachine" &&
                    !known(StateMachineEventDto::class.java, sent.path("data").path("eventType").asText()) -> {
                    unknownMember = true
                    val event = item as? StateMachineAuditEventDto ?: fail("a state-machine event, got $item")
                    assertEquals(sent.path("eventType").asText(), event.eventType, "the event type")
                    assertNull(event.data, "the data of an unknown event type")
                }
                else -> {
                    assertNotNull(item, "a known audit event")
                    val written = tolerant.readTree(tolerant.writeValueAsString(item))
                    assertEquals(withoutNullMembers(declared.path("items")[i]), withoutNullMembers(written), "the known event written back")
                }
            }
        }
        val read = { strict.readValue(value.toString(), EntityAuditEventsResponseDto::class.java) }
        if (unknownMember) {
            assertFailsWith<InvalidTypeIdException>("the read without the two settings", read)
        } else {
            assertFailsWith<UnrecognizedPropertyException>("the read without the two settings", read)
        }
    }

    // withoutAdded returns value with the members at the JSON pointers in
    // added removed.
    private fun withoutAdded(value: JsonNode, added: JsonNode): JsonNode {
        val out = value.deepCopy<JsonNode>()
        added.forEach {
            val ptr = JsonPointer.compile(it.asText())
            val parent = out.at(ptr.head()) as? ObjectNode ?: fail("added ${it.asText()}: no object holds it")
            assertNotNull(parent.remove(ptr.last().matchingProperty), "added ${it.asText()}: no such member")
        }
        return out
    }

    private fun known(base: Class<*>, name: String): Boolean =
        base.getAnnotation(JsonSubTypes::class.java).value.any { it.name == name }

    // Cloud writes non_null, and for these fields a null member and an absent
    // one mean the same: drop object members whose value is null, keep null
    // array entries.
    private fun withoutNullMembers(node: JsonNode): JsonNode = when (node) {
        is ObjectNode -> mapper.createObjectNode().also { out ->
            node.properties().forEach { (k, v) -> if (!v.isNull) out.set<JsonNode>(k, withoutNullMembers(v)) }
        }
        is ArrayNode -> mapper.createArrayNode().also { out -> node.forEach { out.add(withoutNullMembers(it)) } }
        else -> node
    }
}
